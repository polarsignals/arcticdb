package frostdb

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"sync"
	satomic "sync/atomic"

	"github.com/google/btree"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/segmentio/parquet-go"
	"go.uber.org/atomic"

	"github.com/polarsignals/frostdb/dynparquet"
)

type Granule struct {
	metadata GranuleMetadata

	parts       *PartList
	tableConfig *TableConfig

	granulesCreated prometheus.Counter

	// newGranules are the granules that were created after a split
	newGranules []*Granule
}

// GranuleMetadata is the metadata for a granule.
type GranuleMetadata struct {
	// least is the row that exists within the Granule that is the least.
	// This is used for quick insertion into the btree, without requiring an iterator
	least satomic.Pointer[dynparquet.DynamicRow]

	// min contains the minimum value found for each column in the granule. It is used during iteration to validate if the granule contains interesting data
	minlock sync.RWMutex
	min     map[string]*parquet.Value
	// max contains the maximum value found for each column in the granule. It is used during iteration to validate if the granule contains interesting data
	maxlock sync.RWMutex
	max     map[string]*parquet.Value

	// size is the raw commited, and uncommited size of the granule. It is used as a suggestion for potential compaction
	size *atomic.Uint64

	// pruned indicates if this Granule is longer found in the index
	pruned *atomic.Uint64
}

func NewGranule(granulesCreated prometheus.Counter, tableConfig *TableConfig, firstPart *Part) (*Granule, error) {
	g := &Granule{
		granulesCreated: granulesCreated,
		parts:           NewPartList(satomic.Pointer[Node]{}, 0, None),
		tableConfig:     tableConfig,

		metadata: GranuleMetadata{
			min:    map[string]*parquet.Value{},
			max:    map[string]*parquet.Value{},
			least:  satomic.Pointer[dynparquet.DynamicRow]{},
			size:   atomic.NewUint64(0),
			pruned: atomic.NewUint64(0),
		},
	}

	// Find the "smallest" row
	if firstPart != nil {
		g.metadata.size = atomic.NewUint64(uint64(firstPart.Buf.ParquetFile().Size()))
		g.parts.Prepend(firstPart)
		least, err := firstPart.Least()
		if err != nil {
			return nil, err
		}
		g.metadata.least.Store(least)

		// Set the minmaxes on the new granule
		if err := g.minmaxes(firstPart); err != nil {
			return nil, err
		}
	}

	granulesCreated.Inc()
	return g, nil
}

func (g *Granule) addPart(p *Part, r *dynparquet.DynamicRow) (uint64, error) {
	rows := p.Buf.NumRows()
	if rows == 0 {
		return g.metadata.size.Load(), nil
	}
	node := g.parts.Prepend(p)

	newSize := g.metadata.size.Add(uint64(p.Buf.ParquetFile().NumRows()))

	for {
		least := g.metadata.least.Load()
		if least == nil || g.tableConfig.schema.RowLessThan(r, (*dynparquet.DynamicRow)(least)) {
			if g.metadata.least.CompareAndSwap(least, r) {
				break
			}
		} else {
			break
		}
	}

	// Set the minmaxes for the granule
	if err := g.minmaxes(p); err != nil {
		return 0, err
	}

	// If the prepend returned that we're adding to the compacted list; then we need to propogate the Part to the new granules
	if node.sentinel == Compacted {
		err := addPartToGranule(g.newGranules, p)
		if err != nil {
			return 0, err
		}
	}

	return newSize, nil
}

// AddPart returns the new size of the Granule.
func (g *Granule) AddPart(p *Part) (uint64, error) {
	rowBuf := &dynparquet.DynamicRows{Rows: make([]parquet.Row, 1)}
	reader := p.Buf.DynamicRowGroup(0).DynamicRows()
	n, err := reader.ReadRows(rowBuf)
	if err != nil {
		return 0, fmt.Errorf("read first row of part: %w", err)
	}
	if n != 1 {
		return 0, fmt.Errorf("expected to read exactly 1 row, but read %d", n)
	}
	r := rowBuf.GetCopy(0)
	if err := reader.Close(); err != nil {
		return 0, err
	}

	return g.addPart(p, r)
}

// split a granule into n granules. With the last granule containing the remainder.
// Returns the granules in order.
// This assumes the Granule has had its parts merged into a single part.
func (g *Granule) split(tx uint64, count int) ([]*Granule, error) {
	// Get the first part in the granule's part list.
	var p *Part
	g.parts.Iterate(func(part *Part) bool {
		// Since all parts are already merged into one, this iterator will only
		// iterate over the one and only part.
		p = part
		return false
	})

	// Build all the new granules
	granules := make([]*Granule, 0, count)

	// TODO: Buffers should be able to efficiently slice themselves.
	var (
		rowBuf = make([]parquet.Row, 1)
		b      *bytes.Buffer
		w      *dynparquet.PooledWriter
	)
	b = bytes.NewBuffer(nil)
	w, err := g.tableConfig.schema.GetWriter(b, p.Buf.DynamicColumns())
	if err != nil {
		return nil, ErrCreateSchemaWriter{err}
	}

	rowsWritten := 0
	n := int(p.Buf.NumRows()) / count

	f := p.Buf.ParquetFile()
	for _, rowGroup := range f.RowGroups() {
		rows := rowGroup.Rows()
		for {
			_, err = rows.ReadRows(rowBuf)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, ErrReadRow{err}
			}

			_, err = w.WriteRows(rowBuf)
			if err != nil {
				return nil, ErrWriteRow{err}
			}
			rowsWritten++

			if rowsWritten == n && len(granules) != count-1 { // If we have n rows, and aren't on the last granule create a new granule
				err = w.Close()
				if err != nil {
					return nil, fmt.Errorf("close writer: %w", err)
				}
				r, err := dynparquet.ReaderFromBytes(b.Bytes())
				if err != nil {
					return nil, fmt.Errorf("create reader: %w", err)
				}
				gran, err := NewGranule(g.granulesCreated, g.tableConfig, NewPart(tx, r))
				if err != nil {
					return nil, fmt.Errorf("new granule failed: %w", err)
				}
				granules = append(granules, gran)
				b = bytes.NewBuffer(nil)
				g.tableConfig.schema.PutWriter(w)
				w, err = g.tableConfig.schema.GetWriter(b, p.Buf.DynamicColumns())
				if err != nil {
					return nil, ErrCreateSchemaWriter{err}
				}
				rowsWritten = 0
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	if rowsWritten > 0 {
		// Save the remaining Granule
		err = w.Close()
		if err != nil {
			return nil, fmt.Errorf("close last writer: %w", err)
		}
		g.tableConfig.schema.PutWriter(w)

		r, err := dynparquet.ReaderFromBytes(b.Bytes())
		if err != nil {
			return nil, fmt.Errorf("create last reader: %w", err)
		}
		gran, err := NewGranule(g.granulesCreated, g.tableConfig, NewPart(tx, r))
		if err != nil {
			return nil, fmt.Errorf("new granule failed: %w", err)
		}
		granules = append(granules, gran)
	}

	return granules, nil
}

// PartBuffersForTx returns the PartBuffers for the given transaction constraints.
func (g *Granule) PartBuffersForTx(watermark uint64, iterator func(*dynparquet.SerializedBuffer) bool) {
	g.parts.Iterate(func(p *Part) bool {
		// Don't iterate over parts from an uncompleted transaction
		if p.tx > watermark {
			return true
		}

		return iterator(p.Buf)
	})
}

// Less implements the btree.Item interface.
func (g *Granule) Less(than btree.Item) bool {
	return g.tableConfig.schema.RowLessThan(g.Least(), than.(*Granule).Least())
}

// Least returns the least row in a Granule.
func (g *Granule) Least() *dynparquet.DynamicRow {
	return (*dynparquet.DynamicRow)(g.metadata.least.Load())
}

// minmaxes finds the mins and maxes of every column in a part.
func (g *Granule) minmaxes(p *Part) error {
	f := p.Buf.ParquetFile()

	for _, rowGroup := range f.RowGroups() {
		for _, columnChunk := range rowGroup.ColumnChunks() {
			idx := columnChunk.ColumnIndex()
			minvalues := make([]parquet.Value, 0, idx.NumPages())
			maxvalues := make([]parquet.Value, 0, idx.NumPages())
			for k := 0; k < idx.NumPages(); k++ {
				minvalues = append(minvalues, idx.MinValue(k))
				maxvalues = append(maxvalues, idx.MaxValue(k))
			}

			// Check for min
			min := findMin(columnChunk.Type(), minvalues)

			g.metadata.minlock.RLock()
			val := g.metadata.min[rowGroup.Schema().Fields()[columnChunk.Column()].Name()]
			g.metadata.minlock.RUnlock()
			if val == nil || columnChunk.Type().Compare(*val, *min) == 1 {
				if !min.IsNull() {
					g.metadata.minlock.Lock() // Check again after acquiring the write lock
					if val := g.metadata.min[rowGroup.Schema().Fields()[columnChunk.Column()].Name()]; val == nil || columnChunk.Type().Compare(*val, *min) == 1 {
						g.metadata.min[rowGroup.Schema().Fields()[columnChunk.Column()].Name()] = min
					}
					g.metadata.minlock.Unlock()
				}
			}

			// Check for max
			max := findMax(columnChunk.Type(), maxvalues)
			g.metadata.maxlock.RLock()
			val = g.metadata.max[rowGroup.Schema().Fields()[columnChunk.Column()].Name()]
			g.metadata.maxlock.RUnlock()
			if val == nil || columnChunk.Type().Compare(*val, *max) == -1 {
				if !max.IsNull() {
					g.metadata.maxlock.Lock() // Check again after acquiring the write lock
					if val := g.metadata.max[rowGroup.Schema().Fields()[columnChunk.Column()].Name()]; val == nil || columnChunk.Type().Compare(*val, *max) == -1 {
						g.metadata.max[rowGroup.Schema().Fields()[columnChunk.Column()].Name()] = max
					}
					g.metadata.maxlock.Unlock()
				}
			}
		}
	}

	return nil
}

func find(minmax int, t parquet.Type, values []parquet.Value) *parquet.Value {
	if len(values) == 0 {
		return nil
	}

	val := values[0]
	for i := 1; i < len(values); i++ {
		if t.Compare(val, values[i]) != minmax {
			val = values[i]
		}
	}

	return &val
}

func findMax(t parquet.Type, values []parquet.Value) *parquet.Value {
	return find(1, t, values)
}

func findMin(t parquet.Type, values []parquet.Value) *parquet.Value {
	return find(-1, t, values)
}

// Schema implements the Particulate interface. It generates a parquet.Schema from the min/max fields of the Granule.
func (g *Granule) Schema() *parquet.Schema {
	group := parquet.Group{}
	g.metadata.maxlock.RLock()
	defer g.metadata.maxlock.RUnlock()
	for name, v := range g.metadata.max {
		switch v.Kind() {
		case parquet.Int32:
			group[name] = parquet.Int(32)
		case parquet.Int64:
			group[name] = parquet.Int(64)
		case parquet.Float:
			group[name] = parquet.Leaf(parquet.FloatType)
		case parquet.Double:
			group[name] = parquet.Leaf(parquet.DoubleType)
		case parquet.ByteArray:
			group[name] = parquet.String()
		case parquet.FixedLenByteArray:
			group[name] = parquet.Leaf(parquet.ByteArrayType)
		default:
			group[name] = parquet.Leaf(parquet.DoubleType)
		}
	}
	return parquet.NewSchema("granule", group)
}

// ColumnChunks implements the Particulate interface.
func (g *Granule) ColumnChunks() []parquet.ColumnChunk {
	var chunks []parquet.ColumnChunk
	g.metadata.maxlock.RLock()
	defer g.metadata.maxlock.RUnlock()
	g.metadata.minlock.RLock()
	defer g.metadata.minlock.RUnlock()

	names := []string{}
	for name := range g.metadata.max {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		chunks = append(chunks, VirtualSparseColumnChunk{
			i: VirtualSparseColumnIndex{
				Min: *g.metadata.min[name],
				Max: *g.metadata.max[name],
			},
		})
	}

	return chunks
}
