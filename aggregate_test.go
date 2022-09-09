package frostdb

import (
	"context"
	"testing"

	"github.com/apache/arrow/go/v8/arrow"
	"github.com/apache/arrow/go/v8/arrow/array"
	"github.com/apache/arrow/go/v8/arrow/memory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/polarsignals/frostdb/dynparquet"
	"github.com/polarsignals/frostdb/query"
	"github.com/polarsignals/frostdb/query/logicalplan"
)

func TestAggregate(t *testing.T) {
	config := NewTableConfig(
		dynparquet.NewSampleSchema(),
	)

	logger := newTestLogger(t)

	c, err := New(WithLogger(logger))
	require.NoError(t, err)
	db, err := c.DB(context.Background(), "test")
	require.NoError(t, err)
	table, err := db.Table("test", config)
	require.NoError(t, err)

	samples := dynparquet.Samples{{
		Labels: []dynparquet.Label{
			{Name: "label1", Value: "value1"},
			{Name: "label2", Value: "value2"},
		},
		Stacktrace: []uuid.UUID{
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1},
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x2},
		},
		Timestamp: 1,
		Value:     1,
	}, {
		Labels: []dynparquet.Label{
			{Name: "label1", Value: "value2"},
			{Name: "label2", Value: "value2"},
			{Name: "label3", Value: "value3"},
		},
		Stacktrace: []uuid.UUID{
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1},
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x2},
		},
		Timestamp: 2,
		Value:     2,
	}, {
		Labels: []dynparquet.Label{
			{Name: "label1", Value: "value3"},
			{Name: "label2", Value: "value2"},
			{Name: "label4", Value: "value4"},
		},
		Stacktrace: []uuid.UUID{
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1},
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x2},
		},
		Timestamp: 3,
		Value:     3,
	}}

	buf, err := samples.ToBuffer(table.Schema())
	require.NoError(t, err)

	_, err = table.InsertBuffer(context.Background(), buf)
	require.NoError(t, err)

	// Ensure all transactions are completed
	table.Sync()

	engine := query.NewEngine(
		memory.NewGoAllocator(),
		db.TableProvider(),
	)

	for _, testCase := range []struct {
		fn             func(logicalplan.Expr) *logicalplan.AggregationFunction
		alias          string
		expInt64Vals   []int64
		expFloat64Vals []float64
	}{
		{
			fn:           logicalplan.Sum,
			alias:        "value_sum",
			expInt64Vals: []int64{6},
		},
		{
			fn:           logicalplan.Max,
			alias:        "value_max",
			expInt64Vals: []int64{3},
		},
		{
			fn:           logicalplan.Count,
			alias:        "value_count",
			expInt64Vals: []int64{3},
		},
		{
			fn:             logicalplan.Avg,
			alias:          "value_avg",
			expFloat64Vals: []float64{2},
		},
	} {
		t.Run(testCase.alias, func(t *testing.T) {
			var res arrow.Record
			err = engine.ScanTable("test").
				Aggregate(
					testCase.fn(logicalplan.Col("value")).Alias(testCase.alias),
					logicalplan.Col("labels.label2"),
				).Execute(context.Background(), func(ctx context.Context, r arrow.Record) error {
				r.Retain()
				res = r
				return nil
			})
			require.NoError(t, err)
			defer res.Release()

			cols := res.Columns()
			require.Equal(t, 2, len(cols))
			for i, col := range cols {
				require.Equal(t, 1, col.Len(), "unexpected number of values in column %s", res.Schema().Field(i).Name)
			}
			if len(testCase.expInt64Vals) > 0 {
				require.Equal(t, testCase.expInt64Vals, cols[len(cols)-1].(*array.Int64).Int64Values())
			} else if len(testCase.expFloat64Vals) > 0 {
				require.Equal(t, testCase.expFloat64Vals, cols[len(cols)-1].(*array.Float64).Float64Values())
			}
		})
	}
}

func TestAggregateNils(t *testing.T) {
	config := NewTableConfig(
		dynparquet.NewSampleSchema(),
	)

	logger := newTestLogger(t)

	c, err := New(
		WithLogger(logger),
	)
	require.NoError(t, err)
	db, err := c.DB(context.Background(), "test")
	require.NoError(t, err)
	table, err := db.Table("test", config)
	require.NoError(t, err)

	samples := dynparquet.Samples{{
		Labels: []dynparquet.Label{
			{Name: "label1", Value: "value1"},
		},
		Stacktrace: []uuid.UUID{
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1},
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x2},
		},
		Timestamp: 1,
		Value:     1,
	}, {
		Labels: []dynparquet.Label{
			{Name: "label2", Value: "value2"},
		},
		Stacktrace: []uuid.UUID{
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1},
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x2},
		},
		Timestamp: 2,
		Value:     2,
	}, {
		Labels: []dynparquet.Label{
			{Name: "label2", Value: "value2"},
		},
		Stacktrace: []uuid.UUID{
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1},
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x2},
		},
		Timestamp: 3,
		Value:     3,
	}}

	buf, err := samples.ToBuffer(table.Schema())
	require.NoError(t, err)

	_, err = table.InsertBuffer(context.Background(), buf)
	require.NoError(t, err)

	engine := query.NewEngine(
		memory.NewGoAllocator(),
		db.TableProvider(),
	)

	for _, testCase := range []struct {
		fn             func(logicalplan.Expr) *logicalplan.AggregationFunction
		alias          string
		expInt64Vals   []int64
		expFloat64Vals []float64
	}{
		{
			fn:           logicalplan.Sum,
			alias:        "value_sum",
			expInt64Vals: []int64{1, 5},
		},
		{
			fn:           logicalplan.Max,
			alias:        "value_max",
			expInt64Vals: []int64{1, 3},
		},
		{
			fn:           logicalplan.Count,
			alias:        "value_count",
			expInt64Vals: []int64{1, 2},
		},
		{
			fn:             logicalplan.Avg,
			alias:          "value_avg",
			expFloat64Vals: []float64{1, 2.5},
		},
	} {
		t.Run(testCase.alias, func(t *testing.T) {
			var res arrow.Record
			err = engine.ScanTable("test").
				Aggregate(
					testCase.fn(logicalplan.Col("value")).Alias(testCase.alias),
					logicalplan.Col("labels.label2"),
				).Execute(context.Background(), func(ctx context.Context, r arrow.Record) error {
				r.Retain()
				res = r
				return nil
			})
			require.NoError(t, err)
			defer res.Release()

			cols := res.Columns()
			require.Equal(t, 2, len(cols))
			for i, col := range cols {
				require.Equal(t, 2, col.Len(), "unexpected number of values in column %s", res.Schema().Field(i).Name)
			}
			if len(testCase.expInt64Vals) > 0 {
				require.Equal(t, testCase.expInt64Vals, cols[len(cols)-1].(*array.Int64).Int64Values())
			} else if len(testCase.expFloat64Vals) > 0 {
				require.Equal(t, testCase.expFloat64Vals, cols[len(cols)-1].(*array.Float64).Float64Values())
			}
		})
	}
}

func TestAggregateInconsistentSchema(t *testing.T) {
	config := NewTableConfig(
		dynparquet.NewSampleSchema(),
	)

	logger := newTestLogger(t)

	c, err := New(
		WithLogger(logger),
	)
	require.NoError(t, err)
	db, err := c.DB(context.Background(), "test")
	require.NoError(t, err)
	table, err := db.Table("test", config)
	require.NoError(t, err)

	samples := dynparquet.Samples{{
		Labels: []dynparquet.Label{
			{Name: "label1", Value: "value1"},
		},
		Stacktrace: []uuid.UUID{
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1},
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x2},
		},
		Timestamp: 1,
		Value:     1,
	}, {
		Labels: []dynparquet.Label{
			{Name: "label2", Value: "value2"},
		},
		Stacktrace: []uuid.UUID{
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1},
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x2},
		},
		Timestamp: 2,
		Value:     2,
	}, {
		Labels: []dynparquet.Label{
			{Name: "label2", Value: "value2"},
		},
		Stacktrace: []uuid.UUID{
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1},
			{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x2},
		},
		Timestamp: 3,
		Value:     3,
	}}

	for i := range samples {
		buf, err := samples[i : i+1].ToBuffer(table.Schema())
		require.NoError(t, err)

		_, err = table.InsertBuffer(context.Background(), buf)
		require.NoError(t, err)
	}

	engine := query.NewEngine(
		memory.NewGoAllocator(),
		db.TableProvider(),
	)

	for _, testCase := range []struct {
		fn             func(logicalplan.Expr) *logicalplan.AggregationFunction
		alias          string
		expInt64Vals   []int64
		expFloat64Vals []float64
	}{
		{
			fn:           logicalplan.Sum,
			alias:        "value_sum",
			expInt64Vals: []int64{5, 1},
		},
		{
			fn:           logicalplan.Max,
			alias:        "value_max",
			expInt64Vals: []int64{3, 1},
		},
		{
			fn:           logicalplan.Count,
			alias:        "value_count",
			expInt64Vals: []int64{2, 1},
		},
		{
			fn:             logicalplan.Avg,
			alias:          "value_avg",
			expFloat64Vals: []float64{2.5, 1},
		},
	} {
		t.Run(testCase.alias, func(t *testing.T) {
			var res arrow.Record
			err = engine.ScanTable("test").
				Aggregate(
					testCase.fn(logicalplan.Col("value")).Alias(testCase.alias),
					logicalplan.Col("labels.label2"),
				).Execute(context.Background(), func(ctx context.Context, r arrow.Record) error {
				r.Retain()
				res = r
				return nil
			})
			require.NoError(t, err)
			defer res.Release()

			cols := res.Columns()
			require.Equal(t, 2, len(cols))
			for i, col := range cols {
				require.Equal(t, 2, col.Len(), "unexpected number of values in column %s", res.Schema().Field(i).Name)
			}
			if len(testCase.expInt64Vals) > 0 {
				require.Equal(t, testCase.expInt64Vals, cols[len(cols)-1].(*array.Int64).Int64Values())
			} else if len(testCase.expFloat64Vals) > 0 {
				require.Equal(t, testCase.expFloat64Vals, cols[len(cols)-1].(*array.Float64).Float64Values())
			}
		})
	}
}
