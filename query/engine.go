package query

import (
	"github.com/apache/arrow/go/v8/arrow"
	"github.com/apache/arrow/go/v8/arrow/memory"

	"github.com/polarsignals/arcticdb/dynparquet"
	"github.com/polarsignals/arcticdb/query/logicalplan"
	"github.com/polarsignals/arcticdb/query/physicalplan"
)

type Engine struct {
	pool          memory.Allocator
	tableProvider logicalplan.TableProvider
}

func NewEngine(
	pool memory.Allocator,
	tableProvider logicalplan.TableProvider,
) *Engine {
	return &Engine{
		pool:          pool,
		tableProvider: tableProvider,
	}
}

type QueryBuilder struct {
	pool        memory.Allocator
	planBuilder logicalplan.Builder
}

func (e *Engine) ScanTable(name string, options ...logicalplan.IterateOption) QueryBuilder {
	return QueryBuilder{
		pool:        e.pool,
		planBuilder: (&logicalplan.Builder{}).Scan(e.tableProvider, name, options...),
	}
}

func (b QueryBuilder) Aggregate(
	aggExpr logicalplan.Expr,
	groupExprs ...logicalplan.ColumnExpr,
) QueryBuilder {
	return QueryBuilder{
		pool:        b.pool,
		planBuilder: b.planBuilder.Aggregate(aggExpr, groupExprs...),
	}
}

func (b QueryBuilder) Filter(
	expr logicalplan.Expr,
) QueryBuilder {
	return QueryBuilder{
		pool:        b.pool,
		planBuilder: b.planBuilder.Filter(expr),
	}
}

func (b QueryBuilder) Distinct(
	expr ...logicalplan.ColumnExpr,
) QueryBuilder {
	return QueryBuilder{
		pool:        b.pool,
		planBuilder: b.planBuilder.Distinct(expr...),
	}
}

func (b QueryBuilder) Project(
	projections ...string,
) QueryBuilder {
	return QueryBuilder{
		pool:        b.pool,
		planBuilder: b.planBuilder.Project(projections...),
	}
}

func (b QueryBuilder) Execute(callback func(r arrow.Record) error) error {
	logicalPlan := b.planBuilder.Build()

	optimizers := []logicalplan.Optimizer{
		&logicalplan.ProjectionPushDown{},
		&logicalplan.FilterPushDown{},
		&logicalplan.DistinctPushDown{},
	}

	for _, optimizer := range optimizers {
		optimizer.Optimize(logicalPlan)
	}

	phyPlan, err := physicalplan.Build(
		b.pool,
		dynparquet.NewSampleSchema(),
		logicalPlan,
	)
	if err != nil {
		return err
	}

	return phyPlan.Execute(b.pool, callback)
}
