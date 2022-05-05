package arcticdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"unsafe"

	"github.com/apache/arrow/go/v8/arrow"
	"github.com/apache/arrow/go/v8/arrow/array"
	"github.com/apache/arrow/go/v8/arrow/memory"
	"github.com/apache/arrow/go/v8/arrow/scalar"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/google/btree"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/segmentio/parquet-go"
	"go.uber.org/atomic"

	"github.com/polarsignals/arcticdb/dynparquet"
	"github.com/polarsignals/arcticdb/pqarrow"
	"github.com/polarsignals/arcticdb/query/logicalplan"
)

var ErrNoSchema = fmt.Errorf("no schema")

type ErrWriteRow struct{ err error }

func (e ErrWriteRow) Error() string { return "failed to write row: " + e.err.Error() }

type ErrReadRow struct{ err error }

func (e ErrReadRow) Error() string { return "failed to read row: " + e.err.Error() }

type ErrCreateSchemaWriter struct{ err error }

func (e ErrCreateSchemaWriter) Error() string {
	return "failed to create schema write: " + e.err.Error()
}

type TableConfig struct {
	schema *dynparquet.Schema
}

func NewTableConfig(
	schema *dynparquet.Schema,
) *TableConfig {
	return &TableConfig{
		schema: schema,
	}
}

type Table struct {
	db      *DB
	metrics *tableMetrics
	logger  log.Logger

	config *TableConfig

	mtx    *sync.RWMutex
	active *TableBlock
}

type TableBlock struct {
	table  *Table
	logger log.Logger

	size  *atomic.Int64
	index *atomic.UnsafePointer // *btree.BTree

	wg  *sync.WaitGroup
	mtx *sync.Mutex
}

type tableMetrics struct {
	granulesCreated  prometheus.Counter
	granulesSplits   prometheus.Counter
	rowsInserted     prometheus.Counter
	zeroRowsInserted prometheus.Counter
	rowInsertSize    prometheus.Histogram
}

func newTable(
	db *DB,
	name string,
	tableConfig *TableConfig,
	reg prometheus.Registerer,
	logger log.Logger,
) (*Table, error) {
	if db.columnStore.indexDegree <= 0 {
		msg := fmt.Sprintf("Table's columnStore index degree must be a positive integer (received %d)", db.columnStore.indexDegree)
		return nil, errors.New(msg)
	}

	if db.columnStore.splitSize < 2 {
		msg := fmt.Sprintf("Table's columnStore splitSize must be a positive integer > 1 (received %d)", db.columnStore.splitSize)
		return nil, errors.New(msg)
	}

	reg = prometheus.WrapRegistererWith(prometheus.Labels{"table": name}, reg)

	t := &Table{
		db:     db,
		config: tableConfig,
		logger: logger,
		mtx:    &sync.RWMutex{},
		metrics: &tableMetrics{
			granulesCreated: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "granules_created",
				Help: "Number of granules created.",
			}),
			granulesSplits: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "granules_splits",
				Help: "Number of granules splits executed.",
			}),
			rowsInserted: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "rows_inserted",
				Help: "Number of rows inserted into table.",
			}),
			zeroRowsInserted: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "zero_rows_inserted",
				Help: "Number of times it was attempted to insert zero rows into the table.",
			}),
			rowInsertSize: promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
				Name:    "row_insert_size",
				Help:    "Size of batch inserts into table.",
				Buckets: prometheus.ExponentialBuckets(1, 2, 10),
			}),
		},
	}

	var err error
	t.active, err = newTableBlock(t)
	if err != nil {
		return nil, err
	}

	promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "index_size",
		Help: "Number of granules in the table index currently.",
	}, func() float64 {
		return float64(t.ActiveBlock().Index().Len())
	})

	promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "active_table_block_size",
		Help: "Size of the active table block in bytes.",
	}, func() float64 {
		return float64(t.ActiveBlock().Size())
	})

	return t, nil
}

func (t *Table) RotateBlock() error {
	tb, err := newTableBlock(t)
	if err != nil {
		return err
	}
	t.mtx.Lock()
	defer t.mtx.Unlock()

	t.active = tb
	return nil
}

func (t *Table) ActiveBlock() *TableBlock {
	t.mtx.RLock()
	defer t.mtx.RUnlock()

	return t.active
}

func (t *Table) Schema() *dynparquet.Schema {
	return t.config.schema
}

func (t *Table) Sync() {
	t.ActiveBlock().Sync()
}

func (t *Table) InsertBuffer(buf *dynparquet.Buffer) (uint64, error) {
	b, err := t.config.schema.SerializeBuffer(buf)
	if err != nil {
		return 0, fmt.Errorf("serialize buffer: %w", err)
	}

	return t.Insert(b)
}

func (t *Table) Insert(buf []byte) (uint64, error) {
	tx, _, commit := t.db.begin()
	defer commit()

	serBuf, err := dynparquet.ReaderFromBytes(buf)
	if err != nil {
		return 0, fmt.Errorf("deserialize buffer: %w", err)
	}

	block := t.ActiveBlock()
	err = block.Insert(tx, serBuf)
	if err != nil {
		return 0, err
	}

	if block.Size() > t.db.columnStore.activeMemorySize {
		level.Debug(t.logger).Log("msg", "rotating block")
		if err := t.RotateBlock(); err != nil {
			return 0, fmt.Errorf("failed to rotate block: %w", err)
		}
		level.Debug(t.logger).Log("msg", "done rotating block")
		go func() {
			level.Debug(t.logger).Log("msg", "syncing block")
			block.wg.Wait()
			level.Debug(t.logger).Log("msg", "done syncing block")
		}()
	}

	return tx, nil
}

// Iterator iterates in order over all granules in the table. It stops iterating when the iterator function returns false.
func (t *Table) Iterator(
	pool memory.Allocator,
	projections []logicalplan.ColumnMatcher,
	filterExpr logicalplan.Expr,
	distinctColumns []logicalplan.ColumnMatcher,
	iterator func(r arrow.Record) error,
) error {
	filter, err := booleanExpr(filterExpr)
	if err != nil {
		return err
	}

	rowGroups := []dynparquet.DynamicRowGroup{}
	err = t.ActiveBlock().RowGroupIterator(filterExpr, filter, func(rg dynparquet.DynamicRowGroup) bool {
		rowGroups = append(rowGroups, rg)
		return true
	})
	if err != nil {
		return err
	}

	// Previously we sorted all row groups into a single row group here,
	// but it turns out that none of the downstream uses actually rely on
	// the sorting so it's not worth it in the general case. Physical plans
	// can decide to sort if they need to in order to exploit the
	// characteristics of sorted data.
	for _, rg := range rowGroups {
		var record arrow.Record
		record, err = pqarrow.ParquetRowGroupToArrowRecord(
			pool,
			rg,
			projections,
			filterExpr,
			distinctColumns,
		)
		if err != nil {
			return err
		}
		err = iterator(record)
		record.Release()
		if err != nil {
			return err
		}
	}

	return nil
}

// SchemaIterator iterates in order over all granules in the table and returns
// all the schemas seen across the table.
func (t *Table) SchemaIterator(
	pool memory.Allocator,
	projections []logicalplan.ColumnMatcher,
	filterExpr logicalplan.Expr,
	distinctColumns []logicalplan.ColumnMatcher,
	iterator func(r arrow.Record) error,
) error {
	filter, err := booleanExpr(filterExpr)
	if err != nil {
		return err
	}

	rowGroups := []dynparquet.DynamicRowGroup{}
	err = t.ActiveBlock().RowGroupIterator(nil, filter, func(rg dynparquet.DynamicRowGroup) bool {
		rowGroups = append(rowGroups, rg)
		return true
	})
	if err != nil {
		return err
	}

	schema := arrow.NewSchema(
		[]arrow.Field{
			{Name: "name", Type: arrow.BinaryTypes.String},
		},
		nil,
	)
	for _, rg := range rowGroups {
		b := array.NewRecordBuilder(pool, schema)

		parquetFields := rg.Schema().Fields()
		fieldNames := make([]string, 0, len(parquetFields))
		for _, f := range parquetFields {
			fieldNames = append(fieldNames, f.Name())
		}

		b.Field(0).(*array.StringBuilder).AppendValues(fieldNames, nil)

		record := b.NewRecord()
		err = iterator(record)
		record.Release()
		b.Release()
		if err != nil {
			return err
		}
	}

	return err
}

func newTableBlock(table *Table) (*TableBlock, error) {
	index := btree.New(table.db.columnStore.indexDegree)
	tb := &TableBlock{
		table:  table,
		index:  atomic.NewUnsafePointer(unsafe.Pointer(index)),
		wg:     &sync.WaitGroup{},
		mtx:    &sync.Mutex{},
		size:   atomic.NewInt64(0),
		logger: table.logger,
	}

	g, err := NewGranule(tb.table.metrics.granulesCreated, tb.table.config, nil)
	if err != nil {
		return nil, fmt.Errorf("new granule failed: %w", err)
	}
	(*btree.BTree)(tb.index.Load()).ReplaceOrInsert(g)

	return tb, nil
}

// Sync the table. This will return once all split operations have completed.
// Currently it does not prevent new inserts from happening, so this is only
// safe to rely on if you control all writers. In the future we may need to add a way to
// block new writes as well.
func (t *TableBlock) Sync() {
	t.wg.Wait()
}

func (t *TableBlock) Insert(tx uint64, buf *dynparquet.SerializedBuffer) error {
	defer func() {
		t.table.metrics.rowsInserted.Add(float64(buf.NumRows()))
		t.table.metrics.rowInsertSize.Observe(float64(buf.NumRows()))
	}()

	if buf.NumRows() == 0 {
		t.table.metrics.zeroRowsInserted.Add(float64(buf.NumRows()))
		return nil
	}

	rowsToInsertPerGranule, err := t.splitRowsByGranule(buf)
	if err != nil {
		return fmt.Errorf("failed to split rows by granule: %w", err)
	}

	for granule, serBuf := range rowsToInsertPerGranule {
		card, err := granule.AddPart(NewPart(tx, serBuf))
		if err != nil {
			return fmt.Errorf("failed to add part to granule: %w", err)
		}
		if card >= uint64(t.table.db.columnStore.granuleSize) {
			t.wg.Add(1)
			go t.compact(granule)
		}
		t.size.Add(serBuf.ParquetFile().Size())
	}

	return nil
}

func (t *TableBlock) splitGranule(granule *Granule) {
	// Recheck to ensure the granule still needs to be split
	if !granule.metadata.pruned.CAS(0, 1) {
		return
	}

	// Obtain a new tx for this compaction
	tx, watermark, commit := t.table.db.begin()

	// Start compaction by adding sentinel node to parts list
	parts := granule.parts.Sentinel(Compacting)

	bufs := []dynparquet.DynamicRowGroup{}
	remain := []*Part{}

	sizeBefore := int64(0)
	// Convert all the parts into a set of rows
	parts.Iterate(func(p *Part) bool {
		// Don't merge uncompleted transactions
		if p.tx > watermark {
			remain = append(remain, p)
			return true
		}

		for i := 0; i < p.Buf.NumRowGroups(); i++ {
			bufs = append(bufs, p.Buf.DynamicRowGroup(i))
		}

		sizeBefore += p.Buf.ParquetFile().Size()
		return true
	})

	if len(bufs) == 0 { // aborting; nothing to do
		t.abort(commit, granule)
		return
	}

	merge, err := t.table.config.schema.MergeDynamicRowGroups(bufs)
	if err != nil {
		t.abort(commit, granule)
		level.Error(t.logger).Log("msg", "failed to merge dynamic row groups", "err", err)
		return
	}

	b := bytes.NewBuffer(nil)
	cols := merge.DynamicColumns()
	w, err := t.table.config.schema.NewWriter(b, cols)
	if err != nil {
		t.abort(commit, granule)
		level.Error(t.logger).Log("msg", "failed to create new schema writer", "err", err)
		return
	}

	rows := merge.Rows()
	n := 0
	for {
		row, err := rows.ReadRow(nil)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.abort(commit, granule)
			level.Error(t.logger).Log("msg", "error reading rows", "err", err)
			return
		}
		err = w.WriteRow(row)
		if err != nil {
			t.abort(commit, granule)
			level.Error(t.logger).Log("msg", "error writing rows", "err", err)
			return
		}
		n++
	}

	err = w.Close()
	if err != nil {
		t.abort(commit, granule)
		level.Error(t.logger).Log("msg", "error closing schema writer", "err", err)
		return
	}

	if n < t.table.db.columnStore.granuleSize { // It's possible to have a Granule marked for compaction but all the parts in it aren't completed tx's yet
		t.abort(commit, granule)
		return
	}

	serBuf, err := dynparquet.ReaderFromBytes(b.Bytes())
	if err != nil {
		t.abort(commit, granule)
		level.Error(t.logger).Log("msg", "failed to create reader from bytes", "err", err)
		return
	}

	g, err := NewGranule(t.table.metrics.granulesCreated, t.table.config, NewPart(tx, serBuf))
	if err != nil {
		t.abort(commit, granule)
		level.Error(t.logger).Log("msg", "failed to create granule", "err", err)
		return
	}

	granules, err := g.split(tx, t.table.db.columnStore.granuleSize/t.table.db.columnStore.splitSize)
	if err != nil {
		t.abort(commit, granule)
		level.Error(t.logger).Log("msg", "failed to split granule", "err", err)
		return
	}

	// add remaining parts onto new granules
	for _, p := range remain {
		err := addPartToGranule(granules, p)
		if err != nil {
			t.abort(commit, granule)
			level.Error(t.logger).Log("msg", "failed to add part to granule", "err", err)
			return
		}
	}

	// set the newGranules pointer, so new writes will propogate into these new granules
	granule.newGranules = granules

	// Mark compaction complete in the granule; this will cause new writes to start using the newGranules pointer
	parts = granule.parts.Sentinel(Compacted)

	// Now we need to copy any new parts that happened while we were compacting
	parts.Iterate(func(p *Part) bool {
		err = addPartToGranule(granules, p)
		if err != nil {
			return false
		}
		return true
	})
	if err != nil {
		t.abort(commit, granule)
		level.Error(t.logger).Log("msg", "failed to add part to granule", "err", err)
		return
	}

	// commit our compacted writes.
	// Do this here to avoid a small race condition where we swap the index, and before what was previously a defer commit() would allow a read
	// to not find the compacted parts
	commit()

	for {
		curIndex := t.Index()
		t.mtx.Lock()
		index := curIndex.Clone() // TODO(THOR): we can't clone concurrently
		t.mtx.Unlock()

		deleted := index.Delete(granule)
		if deleted == nil {
			level.Error(t.logger).Log("msg", "failed to delete granule during split")
		}

		for _, g := range granules {
			if dupe := index.ReplaceOrInsert(g); dupe != nil {
				level.Error(t.logger).Log("duplicate insert performed")
			}
		}

		// Point to the new index
		if t.index.CAS(t.index.Load(), unsafe.Pointer(index)) {
			sizeDiff := serBuf.ParquetFile().Size() - sizeBefore
			t.size.Add(sizeDiff)
			return
		}
	}
}

// Iterator iterates in order over all granules in the table. It stops iterating when the iterator function returns false.
func (t *TableBlock) RowGroupIterator(
	filterExpr logicalplan.Expr,
	filter TrueNegativeFilter,
	iterator func(rg dynparquet.DynamicRowGroup) bool,
) error {
	index := t.Index()
	watermark := t.table.db.beginRead()

	var err error
	index.Ascend(func(i btree.Item) bool {
		g := i.(*Granule)

		// Check if the entire granule can be skipped due to the filter expr
		if filterGranule(filterExpr, g) {
			return true
		}

		g.PartBuffersForTx(watermark, func(buf *dynparquet.SerializedBuffer) bool {
			f := buf.ParquetFile()
			for i := 0; i < f.NumRowGroups(); i++ {
				rg := buf.DynamicRowGroup(i)
				var mayContainUsefulData bool
				mayContainUsefulData, err = filter.Eval(rg)
				if err != nil {
					return false
				}
				if mayContainUsefulData {
					continu := iterator(rg)
					if !continu {
						return false
					}
				}
			}
			return true
		})

		return true
	})

	return err
}

// Size returns the cumulative size of all buffers in the table. This is roughly the size of the table in bytes.
func (t *TableBlock) Size() int64 {
	return t.size.Load()
}

// Index provides atomic access to the table index.
func (t *TableBlock) Index() *btree.BTree {
	return (*btree.BTree)(t.index.Load())
}

func (t *TableBlock) granuleIterator(iterator func(g *Granule) bool) {
	t.Index().Ascend(func(i btree.Item) bool {
		g := i.(*Granule)
		return iterator(g)
	})
}

func (t *TableBlock) splitRowsByGranule(buf *dynparquet.SerializedBuffer) (map[*Granule]*dynparquet.SerializedBuffer, error) {
	// Special case: if there is only one granule, insert parts into it until full.
	index := t.Index()
	if index.Len() == 1 {
		b := bytes.NewBuffer(nil)

		cols := buf.DynamicColumns()
		w, err := t.table.config.schema.NewWriter(b, cols)
		if err != nil {
			return nil, ErrCreateSchemaWriter{err}
		}

		rows := buf.Reader()
		for {
			row, err := rows.ReadRow(nil)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, ErrReadRow{err}
			}
			err = w.WriteRow(row)
			if err != nil {
				return nil, ErrWriteRow{err}
			}
		}

		err = w.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to close schema writer: %w", err)
		}

		serBuf, err := dynparquet.ReaderFromBytes(b.Bytes())
		if err != nil {
			return nil, fmt.Errorf("failed to create dynparquet reader: %w", err)
		}

		return map[*Granule]*dynparquet.SerializedBuffer{
			index.Min().(*Granule): serBuf,
		}, nil
	}

	writerByGranule := map[*Granule]*parquet.Writer{}
	bufByGranule := map[*Granule]*bytes.Buffer{}

	// TODO: we might be able to do ascend less than or ascend greater than here?
	rows := buf.DynamicRows()
	var prev *Granule
	exhaustedAllRows := false

	row, err := rows.ReadRow(nil)
	if err != nil {
		return nil, ErrReadRow{err}
	}

	var ascendErr error

	index.Ascend(func(i btree.Item) bool {
		g := i.(*Granule)

		for {
			least := g.Least()
			isLess := t.table.config.schema.RowLessThan(row, least)
			if isLess {
				if prev != nil {
					w, ok := writerByGranule[prev]
					if !ok {
						b := bytes.NewBuffer(nil)
						w, err = t.table.config.schema.NewWriter(b, buf.DynamicColumns())
						if err != nil {
							ascendErr = ErrCreateSchemaWriter{err}
							return false
						}
						writerByGranule[prev] = w
						bufByGranule[prev] = b
					}
					err = w.WriteRow(row.Row)
					if err != nil {
						ascendErr = ErrWriteRow{err}
						return false
					}
					row, err = rows.ReadRow(row)
					if err == io.EOF {
						// All rows accounted for
						exhaustedAllRows = true
						return false
					}
					if err != nil {
						ascendErr = ErrReadRow{err}
						return false
					}
					continue
				}
			}

			// stop at the first granule where this is not the least
			// this might be the correct granule, but we need to check that it isn't the next granule
			prev = g
			return true // continue btree iteration
		}
	})
	if ascendErr != nil {
		return nil, ascendErr
	}

	if !exhaustedAllRows {
		w, ok := writerByGranule[prev]
		if !ok {
			b := bytes.NewBuffer(nil)
			w, err = t.table.config.schema.NewWriter(b, buf.DynamicColumns())
			if err != nil {
				return nil, ErrCreateSchemaWriter{err}
			}
			writerByGranule[prev] = w
			bufByGranule[prev] = b
		}

		// Save any remaining rows that belong into prev
		for {
			err = w.WriteRow(row.Row)
			if err != nil {
				return nil, ErrWriteRow{err}
			}
			row, err = rows.ReadRow(row)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, ErrReadRow{err}
			}
		}
	}

	res := map[*Granule]*dynparquet.SerializedBuffer{}
	for g, w := range writerByGranule {
		err := w.Close()
		if err != nil {
			return nil, err
		}
		res[g], err = dynparquet.ReaderFromBytes(bufByGranule[g].Bytes())
		if err != nil {
			return nil, fmt.Errorf("failed to read from granule buffer: %w", err)
		}
	}

	return res, nil
}

// compact will compact a Granule; should be performed as a background go routine.
func (t *TableBlock) compact(g *Granule) {
	defer t.wg.Done()
	t.splitGranule(g)
}

// addPartToGranule finds the corresponding granule it belongs to in a sorted list of Granules.
func addPartToGranule(granules []*Granule, p *Part) error {
	row, err := p.Buf.DynamicRowGroup(0).DynamicRows().ReadRow(nil)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}

	var prev *Granule
	for _, g := range granules {
		if g.tableConfig.schema.RowLessThan(row, g.Least()) {
			if prev != nil {
				if _, err := prev.AddPart(p); err != nil {
					return err
				}
				return nil
			}
		}
		prev = g
	}

	if prev != nil {
		// Save part to prev
		if _, err := prev.AddPart(p); err != nil {
			return err
		}
	}

	return nil
}

// abort a compaction transaction.
func (t *TableBlock) abort(commit func(), granule *Granule) {
	for {
		if granule.metadata.pruned.CAS(1, 0) { // unmark pruned, so that we can compact it in the future
			commit()
			return
		}
	}
}

func filterGranule(filterExpr logicalplan.Expr, g *Granule) bool {
	if filterExpr == nil {
		return false
	}

	switch expr := filterExpr.(type) {
	case logicalplan.BinaryExpr:
		matchers := expr.Left.ColumnsUsed()
		for column, min := range g.metadata.min {
			for _, matcher := range matchers {
				if matcher.Match(column) {
					switch leftExpr := expr.Left.(type) {
					case logicalplan.BinaryExpr:
						switch leftExpr.Op {
						case logicalplan.GTOp:
							switch literal := leftExpr.Right.(type) {
							case logicalplan.LiteralExpr:
								v := literal.Value.(*scalar.Int64)
								if g.metadata.max[column].Int64() <= v.Value {
									return true
								}
							}
						case logicalplan.LTOp:
							switch literal := leftExpr.Right.(type) {
							case logicalplan.LiteralExpr:
								v := literal.Value.(*scalar.Int64)
								fmt.Println(leftExpr)
								if min.Int64() >= v.Value {
									return true
								}
							}
						}
					}
					switch rightExpr := expr.Right.(type) {
					case logicalplan.BinaryExpr:
						switch rightExpr.Op {
						case logicalplan.GTOp:
							switch literal := rightExpr.Right.(type) {
							case logicalplan.LiteralExpr:
								v := literal.Value.(*scalar.Int64)
								if g.metadata.max[column].Int64() <= v.Value {
									return true
								}
							}
						case logicalplan.LTOp:
							switch literal := rightExpr.Right.(type) {
							case logicalplan.LiteralExpr:
								v := literal.Value.(*scalar.Int64)
								if min.Int64() >= v.Value {
									return true
								}
							}
						}
					}
				}
			}
		}
	}

	return false
}
