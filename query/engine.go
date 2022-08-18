package query

import (
	"context"

	"github.com/apache/arrow/go/v8/arrow"
	"github.com/apache/arrow/go/v8/arrow/memory"

	"github.com/polarsignals/frostdb/query/logicalplan"
	"github.com/polarsignals/frostdb/query/physicalplan"
)

type Builder interface {
	Aggregate(aggExpr logicalplan.Expr, groupExprs ...logicalplan.Expr) Builder
	Filter(expr logicalplan.Expr) Builder
	Distinct(expr ...logicalplan.Expr) Builder
	Project(projections ...logicalplan.Expr) Builder
	Execute(ctx context.Context, callback func(r arrow.Record) error) error
}

type LocalEngine struct {
	pool          memory.Allocator
	tableProvider logicalplan.TableProvider

	optimizations []logicalplan.Optimizer
}

// Hint is a suggestion to be made to the query engine about how it might optimize the query
type Hint func(*LocalEngine)

// ColAsTimestamp is a query engine hint that informs the engine which column to use as a timestamp during historical queries
func ColAsTimestamp(columnName string) func(*LocalEngine) {
	return func(l *LocalEngine) {
		l.optimizations = append(l.optimizations, logicalplan.TimestampColumnOptimization(columnName))
	}
}

func NewEngine(
	pool memory.Allocator,
	tableProvider logicalplan.TableProvider,
	hints ...Hint,
) *LocalEngine {
	e := &LocalEngine{
		pool:          pool,
		tableProvider: tableProvider,
	}

	for _, hint := range hints {
		hint(e)
	}

	return e
}

type LocalQueryBuilder struct {
	pool        memory.Allocator
	planBuilder logicalplan.Builder
}

func (e *LocalEngine) ScanTable(name string) Builder {
	return LocalQueryBuilder{
		pool:        e.pool,
		planBuilder: (&logicalplan.Builder{}).Scan(e.tableProvider, name),
	}
}

func (e *LocalEngine) ScanSchema(name string) Builder {
	return LocalQueryBuilder{
		pool:        e.pool,
		planBuilder: (&logicalplan.Builder{}).ScanSchema(e.tableProvider, name),
	}
}

func (b LocalQueryBuilder) Aggregate(
	aggExpr logicalplan.Expr,
	groupExprs ...logicalplan.Expr,
) Builder {
	return LocalQueryBuilder{
		pool:        b.pool,
		planBuilder: b.planBuilder.Aggregate(aggExpr, groupExprs...),
	}
}

func (b LocalQueryBuilder) Filter(
	expr logicalplan.Expr,
) Builder {
	return LocalQueryBuilder{
		pool:        b.pool,
		planBuilder: b.planBuilder.Filter(expr),
	}
}

func (b LocalQueryBuilder) Distinct(
	expr ...logicalplan.Expr,
) Builder {
	return LocalQueryBuilder{
		pool:        b.pool,
		planBuilder: b.planBuilder.Distinct(expr...),
	}
}

func (b LocalQueryBuilder) Project(
	projections ...logicalplan.Expr,
) Builder {
	return LocalQueryBuilder{
		pool:        b.pool,
		planBuilder: b.planBuilder.Project(projections...),
	}
}

func (b LocalQueryBuilder) Execute(ctx context.Context, callback func(r arrow.Record) error) error {
	logicalPlan, err := b.planBuilder.Build()
	if err != nil {
		return err
	}

	for _, optimizer := range logicalplan.DefaultOptimizers {
		logicalPlan = optimizer.Optimize(logicalPlan)
	}

	phyPlan, err := physicalplan.Build(
		b.pool,
		logicalPlan.InputSchema(),
		logicalPlan,
	)
	if err != nil {
		return err
	}

	return phyPlan.Execute(ctx, b.pool, callback)
}
