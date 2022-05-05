package arcticdb

import (
	"bytes"
	"fmt"
	"io"
	"unsafe"

	"github.com/google/btree"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/segmentio/parquet-go"
	"go.uber.org/atomic"

	"github.com/polarsignals/arcticdb/dynparquet"
)

type Granule struct {
	metadata GranuleMetadata

	parts       *PartList
	tableConfig *TableConfig

	granulesCreated prometheus.Counter

	// newGranules are the granules that were created after a split
	newGranules []*Granule
}

// GranuleMetadata is the metadata for a granule
type GranuleMetadata struct {
	// least is the row that exists within the Granule that is the least.
	// This is used for quick insertion into the btree, without requiring an iterator
	least *atomic.UnsafePointer

	// card is the raw commited, and uncommited cardinality of the granule. It is used as a suggestion for potential compaction
	card *atomic.Uint64

	// pruned indicates if this Granule is longer found in the index
	pruned *atomic.Uint64
}

func NewGranule(granulesCreated prometheus.Counter, tableConfig *TableConfig, firstPart *Part) (*Granule, error) {
	g := &Granule{
		granulesCreated: granulesCreated,
		parts:           NewPartList(nil, 0, None),
		tableConfig:     tableConfig,

		metadata: GranuleMetadata{
			least:  atomic.NewUnsafePointer(nil),
			card:   atomic.NewUint64(0),
			pruned: atomic.NewUint64(0),
		},
	}

	// Find the least column
	if firstPart != nil {
		g.metadata.card = atomic.NewUint64(uint64(firstPart.Buf.NumRows()))
		g.parts.Prepend(firstPart)
		// Since we assume a part is sorted, we need only to look at the first row in each Part
		row, err := firstPart.Buf.DynamicRowGroup(0).DynamicRows().ReadRow(nil)
		if err != nil {
			return nil, err
		}
		g.metadata.least.Store(unsafe.Pointer(row))
	}

	granulesCreated.Inc()
	return g, nil
}

// AddPart returns the new cardinality of the Granule.
func (g *Granule) AddPart(p *Part) (uint64, error) {
	rows := p.Buf.NumRows()
	if rows == 0 {
		return g.metadata.card.Load(), nil
	}
	node := g.parts.Prepend(p)

	newcard := g.metadata.card.Add(uint64(p.Buf.NumRows()))
	r, err := p.Buf.DynamicRowGroup(0).DynamicRows().ReadRow(nil)
	if err != nil {
		return 0, err
	}

	for {
		least := g.metadata.least.Load()
		if least == nil || g.tableConfig.schema.RowLessThan(r, (*dynparquet.DynamicRow)(least)) {
			if g.metadata.least.CAS(least, unsafe.Pointer(r)) {
				break
			}
		} else {
			break
		}
	}

	// If the prepend returned that we're adding to the compacted list; then we need to propogate the Part to the new granules
	if node.sentinel == Compacted {
		err := addPartToGranule(g.newGranules, p)
		if err != nil {
			return 0, err
		}
	}

	return newcard, nil
}

// split a granule into n sized granules. With the last granule containing the remainder.
// Returns the granules in order.
// This assumes the Granule has had its parts merged into a single part.
func (g *Granule) split(tx uint64, n int) ([]*Granule, error) {
	// Get the first part in the granule's part list.
	var p *Part
	g.parts.Iterate(func(part *Part) bool {
		// Since all parts are already merged into one, this iterator will only
		// iterate over the one and only part.
		p = part
		return false
	})
	// How many granules we'll need to build
	count := int(p.Buf.NumRows()) / n

	// Build all the new granules
	granules := make([]*Granule, 0, count)

	// TODO: Buffers should be able to efficiently slice themselves.
	var (
		row parquet.Row
		b   *bytes.Buffer
		w   *parquet.Writer
		err error
	)
	b = bytes.NewBuffer(nil)
	w, err = g.tableConfig.schema.NewWriter(b, p.Buf.DynamicColumns())
	if err != nil {
		return nil, ErrCreateSchemaWriter{err}
	}
	rowsWritten := 0

	f := p.Buf.ParquetFile()
	rowGroups := f.NumRowGroups()
	for i := 0; i < rowGroups; i++ {
		rows := f.RowGroup(i).Rows()
		for {
			row, err = rows.ReadRow(row[:0])
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
			rowsWritten++

			if rowsWritten == n && len(granules) != count-1 { // If we have n rows, and aren't on the last granule, create the n-sized granule
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
				w, err = g.tableConfig.schema.NewWriter(b, p.Buf.DynamicColumns())
				if err != nil {
					return nil, ErrCreateSchemaWriter{err}
				}
				rowsWritten = 0
			}
		}
	}

	if rowsWritten > 0 {
		// Save the remaining Granule
		err = w.Close()
		if err != nil {
			return nil, fmt.Errorf("close last writer: %w", err)
		}
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
