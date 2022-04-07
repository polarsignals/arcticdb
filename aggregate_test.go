package arcticdb

import (
	"testing"

	"github.com/apache/arrow/go/v8/arrow"
	"github.com/apache/arrow/go/v8/arrow/array"
	"github.com/apache/arrow/go/v8/arrow/memory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/polarsignals/arcticdb/dynparquet"
	"github.com/polarsignals/arcticdb/query"
	"github.com/polarsignals/arcticdb/query/logicalplan"
)

func TestAggregate(t *testing.T) {
	config := NewTableConfig(
		dynparquet.NewSampleSchema(),
		8192,
		512*1024*1024,
	)

	c := New(nil)
	db := c.DB("test")
	table := db.Table("test", config, newTestLogger(t))

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

	err = table.Insert(buf)
	require.NoError(t, err)

	// Ensure all transactions are completed
	table.Sync()

	engine := query.NewEngine(
		memory.NewGoAllocator(),
		db.TableProvider(),
	)

	var res arrow.Record
	err = engine.ScanTable("test").
		Aggregate(
			logicalplan.Sum(logicalplan.Col("value")).Alias("value_sum"),
			logicalplan.Col("labels.label2"),
		).Execute(func(r arrow.Record) error {
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
	require.Equal(t, []int64{6}, cols[len(cols)-1].(*array.Int64).Int64Values())
}

func TestAggregateNils(t *testing.T) {
	config := NewTableConfig(
		dynparquet.NewSampleSchema(),
		8192,
		512*1024*1024,
	)

	c := New(nil)
	db := c.DB("test")
	table := db.Table("test", config, newTestLogger(t))

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

	err = table.Insert(buf)
	require.NoError(t, err)

	engine := query.NewEngine(
		memory.NewGoAllocator(),
		db.TableProvider(),
	)

	var res arrow.Record
	err = engine.ScanTable("test").
		Aggregate(
			logicalplan.Sum(logicalplan.Col("value")).Alias("value_sum"),
			logicalplan.Col("labels.label2"),
		).Execute(func(r arrow.Record) error {
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
	require.Equal(t, []int64{1, 5}, cols[len(cols)-1].(*array.Int64).Int64Values())
}
