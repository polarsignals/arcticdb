package frostdb

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/apache/arrow/go/v10/arrow"
	"github.com/apache/arrow/go/v10/arrow/array"
	"github.com/apache/arrow/go/v10/arrow/memory"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/polarsignals/frostdb/dynparquet"
	snapshotpb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/snapshot/v1alpha1"
	"github.com/polarsignals/frostdb/parts"
	"github.com/polarsignals/frostdb/query"
	"github.com/polarsignals/frostdb/query/logicalplan"
)

func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	// Create a new DB with multiple tables and granules with
	// compacted/uncompacted parts that have a mixture of arrow/parquet records.
	t.Run("Empty", func(t *testing.T) {
		c, err := New(
			WithStoragePath(t.TempDir()),
			WithWAL(),
		)
		require.NoError(t, err)
		defer c.Close()

		db, err := c.DB(ctx, "test")
		require.NoError(t, err)

		require.NoError(t, db.snapshot(ctx, db.highWatermark.Load()))

		txBefore := db.highWatermark.Load()
		tx, err := db.loadLatestSnapshot(ctx)
		require.NoError(t, err)
		require.Equal(t, txBefore, tx)
	})

	t.Run("WithData", func(t *testing.T) {
		c, err := New(
			WithStoragePath(t.TempDir()),
			WithWAL(),
		)
		require.NoError(t, err)
		defer c.Close()

		db, err := c.DB(ctx, "test")
		require.NoError(t, err)

		config := NewTableConfig(dynparquet.NewSampleSchema())

		// Pause compactor pool to have control over compactions.
		db.compactorPool.pause()
		table, err := db.Table("table1", config)
		require.NoError(t, err)
		insertSampleRecords(ctx, t, table, 1, 2, 3)
		require.NoError(t, table.EnsureCompaction())
		insertSamples(ctx, t, table, 4, 5, 6)
		insertSampleRecords(ctx, t, table, 7, 8, 9)

		const overrideConfigVal = 1234
		config.rowGroupSize = overrideConfigVal
		table, err = db.Table("table2", config)
		require.NoError(t, err)
		insertSamples(ctx, t, table, 1, 2, 3)
		insertSampleRecords(ctx, t, table, 4, 5, 6)

		config.blockReaderLimit = overrideConfigVal
		_, err = db.Table("empty", config)
		require.NoError(t, err)

		highWatermark := db.highWatermark.Load()

		// Insert a sample that should not be snapshot.
		insertSamples(ctx, t, table, 10)
		require.NoError(t, db.snapshot(ctx, highWatermark))

		// Create another db and verify.
		snapshotDB, err := c.DB(ctx, "testsnapshot")
		require.NoError(t, err)

		snapshotDB.compactorPool.pause()
		// Load the other db's latest snapshot.
		tx, err := snapshotDB.loadLatestSnapshotFromDir(ctx, db.snapshotsDir())
		require.NoError(t, err)
		require.Equal(t, highWatermark, tx)
		require.Equal(t, highWatermark, snapshotDB.highWatermark.Load())

		require.Equal(t, len(db.tables), len(snapshotDB.tables))

		snapshotEngine := query.NewEngine(memory.DefaultAllocator, snapshotDB.TableProvider())

		for _, testCase := range []struct {
			name            string
			expMaxTimestamp int
		}{
			{
				name:            "table1",
				expMaxTimestamp: 9,
			},
			{
				name:            "table2",
				expMaxTimestamp: 6,
			},
			{
				name: "empty",
			},
		} {
			if testCase.expMaxTimestamp != 0 {
				max := []logicalplan.Expr{
					logicalplan.Max(logicalplan.Col("timestamp")),
				}
				require.NoError(
					t,
					snapshotEngine.ScanTable(testCase.name).Aggregate(max, nil).Execute(ctx,
						func(_ context.Context,
							r arrow.Record,
						) error {
							require.Equal(
								t, testCase.expMaxTimestamp, int(r.Column(0).(*array.Int64).Int64Values()[0]),
							)
							return nil
						}),
				)
			}
			// Reset sync.Maps so reflect.DeepEqual can be used below.
			db.tables[testCase.name].config.schema.ResetWriters()
			db.tables[testCase.name].config.schema.ResetBuffers()
			require.Equal(t, db.tables[testCase.name].config, snapshotDB.tables[testCase.name].config)
		}
	})

	t.Run("WithConcurrentWrites", func(t *testing.T) {
		cancelCtx, cancelWrites := context.WithCancel(ctx)

		c, err := New(
			WithStoragePath(t.TempDir()),
			WithWAL(),
		)
		require.NoError(t, err)
		defer c.Close()

		db, err := c.DB(ctx, "test")
		require.NoError(t, err)

		config := NewTableConfig(dynparquet.NewSampleSchema())
		const tableName = "table"
		table, err := db.Table(tableName, config)
		require.NoError(t, err)

		highWatermarkAtStart := db.highWatermark.Load()
		shouldStartSnapshotChan := make(chan struct{})
		var errg errgroup.Group
		errg.Go(func() error {
			ts := int64(highWatermarkAtStart)
			for cancelCtx.Err() == nil {
				tx := insertSamples(ctx, t, table, ts)
				// This check simply ensures that the assumption that inserting
				// timestamp n corresponds to the n+1th transaction (the +1
				// corresponding to table creation). This assumption is required
				// by the snapshot.
				require.Equal(t, uint64(ts+1), tx)
				ts++
				if ts == 10 {
					close(shouldStartSnapshotChan)
				}
			}
			return nil
		})
		// Wait until some writes have happened.
		<-shouldStartSnapshotChan
		defer cancelWrites()
		snapshotDB, err := c.DB(ctx, "testsnapshot")
		require.NoError(t, err)
		require.NoError(t, db.snapshot(ctx, db.highWatermark.Load()))
		snapshotTx, err := snapshotDB.loadLatestSnapshotFromDir(ctx, db.snapshotsDir())
		require.NoError(t, err)
		require.NoError(
			t,
			query.NewEngine(
				memory.DefaultAllocator, snapshotDB.TableProvider(),
			).ScanTable(tableName).Aggregate(
				[]logicalplan.Expr{logicalplan.Max(logicalplan.Col("timestamp"))}, nil,
			).Execute(ctx, func(ctx context.Context, r arrow.Record) error {
				require.Equal(
					t, int(snapshotTx-highWatermarkAtStart), int(r.Column(0).(*array.Int64).Int64Values()[0]),
				)
				return nil
			}),
		)
		cancelWrites()
		require.NoError(t, errg.Wait())
	})
}

// TestSnapshotVerifyFields verifies that struct fields of snapshotted objects
// are explicitly handled/ignored.
func TestSnapshotVerifyFields(t *testing.T) {
	testCases := []struct {
		name           string
		goStruct       any
		protobufStruct any
		resolve        map[string]string
		ignore         map[string]struct{}
	}{
		{
			name:           "TableConfig",
			goStruct:       TableConfig{},
			protobufStruct: snapshotpb.Table_TableConfig{},
		},
		{
			name:           "Part",
			goStruct:       parts.Part{},
			protobufStruct: snapshotpb.Part{},
			ignore: map[string]struct{}{
				// buf and record are serialized separately. Part metadata
				// stores the offsets.
				"buf":    {},
				"record": {},
				// schema is stored in the table config. The dynamic columns
				// are stored in the serialized parquet file.
				"schema": {},
				// minRow and maxRow are calculated at runtime.
				"minRow": {},
				"maxRow": {},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			protoFields := make(map[string]struct{})
			for _, protoField := range reflect.VisibleFields(reflect.TypeOf(tc.protobufStruct)) {
				protoFields[strings.ToLower(protoField.Name)] = struct{}{}
			}
			for _, field := range reflect.VisibleFields(reflect.TypeOf(tc.goStruct)) {
				if _, ok := tc.ignore[field.Name]; ok {
					continue
				}
				name := strings.ToLower(field.Name)
				if resolvedName, ok := tc.resolve[name]; ok {
					name = resolvedName
				}
				if _, ok := protoFields[name]; !ok {
					t.Fatalf("field %s is not handled", name)
				}
			}
		})
	}
}
