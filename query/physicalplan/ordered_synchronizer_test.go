package physicalplan

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/apache/arrow/go/v8/arrow"
	"github.com/apache/arrow/go/v8/arrow/array"
	"github.com/apache/arrow/go/v8/arrow/memory"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/polarsignals/frostdb/pqarrow/builder"
)

func TestOrderedSynchronizer(t *testing.T) {
	var (
		sourceMtx    sync.Mutex
		sourceCursor atomic.Int64
	)
	source := make([]int64, 10000)
	for i := range source {
		source[i] = int64(i)
	}
	// Initialize sourceCursor to -1 so that the first increment is 0.
	sourceCursor.Store(-1)
	const inputs = 8
	osync := NewOrderedSynchronizer(memory.DefaultAllocator, inputs, []int{0})
	expected := int64(0)
	osync.SetNext(&OutputPlan{
		callback: func(_ context.Context, r arrow.Record) error {
			// This is where the result records will be pushed.
			arr := r.Column(0).(*array.Int64)
			for i := 0; i < arr.Len(); i++ {
				require.Equal(t, expected, arr.Value(i))
				expected++
			}
			return nil
		},
	})
	ctx := context.Background()
	var errg errgroup.Group
	for i := 0; i < inputs; i++ {
		errg.Go(func() error {
			b := builder.NewOptInt64Builder(arrow.PrimitiveTypes.Int64)
			for {
				cursor := sourceCursor.Add(1)
				if int(cursor) >= len(source) {
					break
				}
				sourceMtx.Lock()
				b.Append(source[cursor])
				sourceMtx.Unlock()
				arr := b.NewArray()
				if err := osync.Callback(
					ctx,
					array.NewRecord(
						arrow.NewSchema(
							[]arrow.Field{{Type: arr.DataType()}}, nil,
						),
						[]arrow.Array{arr},
						1,
					),
				); err != nil {
					return err
				}
			}
			return nil
		})
	}
	require.NoError(t, errg.Wait())
	// This last check verifies that we read all data.
	require.Equal(t, int(expected), len(source))
}
