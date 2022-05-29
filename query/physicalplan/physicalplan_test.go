package physicalplan

import (
	"context"
	"testing"

	"github.com/apache/arrow/go/v8/arrow"

	"github.com/apache/arrow/go/v8/arrow/memory"
	"github.com/stretchr/testify/require"

	"github.com/polarsignals/arcticdb/dynparquet"
	"github.com/polarsignals/arcticdb/query/logicalplan"
)

type mockTableReader struct {
	schema *dynparquet.Schema
}

func (m *mockTableReader) Schema() *dynparquet.Schema {
	return m.schema
}

func (m *mockTableReader) Iterator(
	ctx context.Context,
	pool memory.Allocator,
	projection []logicalplan.ColumnMatcher,
	filter logicalplan.Expr,
	distinctColumns []logicalplan.ColumnMatcher,
	callback func(r arrow.Record) error,
) error {
	return nil
}

func (m *mockTableReader) SchemaIterator(
	ctx context.Context,
	pool memory.Allocator,
	projection []logicalplan.ColumnMatcher,
	filter logicalplan.Expr,
	distinctColumns []logicalplan.ColumnMatcher,
	callback func(r arrow.Record) error,
) error {
	return nil
}

type mockTableProvider struct {
	schema *dynparquet.Schema
}

func (m *mockTableProvider) GetTable(name string) logicalplan.TableReader {
	return &mockTableReader{
		schema: m.schema,
	}
}

func TestBuildPhysicalPlan(t *testing.T) {
	p, _ := (&logicalplan.Builder{}).
		Scan(&mockTableProvider{schema: dynparquet.NewSampleSchema()}, "table1").
		Filter(logicalplan.Col("labels.test").Eq(logicalplan.Literal("abc"))).
		Aggregate(
			logicalplan.Sum(logicalplan.Col("value")).Alias("value_sum"),
			logicalplan.Col("stacktrace"),
		).
		Project(logicalplan.Col("stacktrace"), logicalplan.Col("value_sum")).
		Build()

	optimizers := []logicalplan.Optimizer{
		&logicalplan.PhysicalProjectionPushDown{},
		&logicalplan.FilterPushDown{},
	}

	for _, optimizer := range optimizers {
		optimizer.Optimize(p)
	}

	_, err := Build(memory.DefaultAllocator, dynparquet.NewSampleSchema(), p)
	require.NoError(t, err)
}
