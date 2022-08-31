package logicalplan

import (
	"context"
	"fmt"
	"strings"

	"github.com/apache/arrow/go/v8/arrow"
	"github.com/apache/arrow/go/v8/arrow/memory"

	"github.com/polarsignals/frostdb/dynparquet"
)

// LogicalPlan is a logical representation of a query. Each LogicalPlan is a
// sub-tree of the query. It is built recursively.
type LogicalPlan struct {
	Input *LogicalPlan

	// Each LogicalPlan struct must only have one of the following.
	SchemaScan  *SchemaScan
	TableScan   *TableScan
	Filter      *Filter
	Distinct    *Distinct
	Projection  *Projection
	Aggregation *Aggregation
}

// IterOptions are a set of options for the TableReader Iterators
// TODO: should we instead use the option pattern? Is that possible with the way the Iterator functions work?
type IterOptions struct {
	PhysicalProjection []Expr
	Projection         []Expr
	Filter             Expr
	DistinctColumns    []Expr
}

func (plan *LogicalPlan) String() string {
	return plan.string(0)
}

func (plan *LogicalPlan) string(indent int) string {
	res := ""
	switch {
	case plan.SchemaScan != nil:
		res = plan.SchemaScan.String()
	case plan.TableScan != nil:
		res = plan.TableScan.String()
	case plan.Filter != nil:
		res = plan.Filter.String()
	case plan.Projection != nil:
		res = plan.Projection.String()
	case plan.Aggregation != nil:
		res = plan.Aggregation.String()
	case plan.Distinct != nil:
		res = plan.Distinct.String()
	default:
		res = "Unknown LogicalPlan"
	}

	res = strings.Repeat("  ", indent) + res
	if plan.Input != nil {
		res += "\n" + plan.Input.string(indent+1)
	}
	return res
}

// TableReader returns the table reader.
func (plan *LogicalPlan) TableReader() TableReader {
	if plan.TableScan != nil {
		return plan.TableScan.TableProvider.GetTable(plan.TableScan.TableName)
	}
	if plan.SchemaScan != nil {
		return plan.SchemaScan.TableProvider.GetTable(plan.SchemaScan.TableName)
	}
	if plan.Input != nil {
		return plan.Input.TableReader()
	}
	return nil
}

// InputSchema returns the schema that the query will execute against.
func (plan *LogicalPlan) InputSchema() *dynparquet.Schema {
	tableReader := plan.TableReader()
	if tableReader != nil {
		return tableReader.Schema()
	}
	return nil
}

type PlanVisitor interface {
	PreVisit(plan *LogicalPlan) bool
	PostVisit(plan *LogicalPlan) bool
}

func (plan *LogicalPlan) Accept(visitor PlanVisitor) bool {
	continu := visitor.PreVisit(plan)
	if !continu {
		return false
	}

	if plan.Input != nil {
		continu = plan.Input.Accept(visitor)
		if !continu {
			return false
		}
	}

	return visitor.PostVisit(plan)
}

type TableReader interface {
	View(ctx context.Context, fn func(ctx context.Context, tx uint64) error) error
	Iterator(
		ctx context.Context,
		tx uint64,
		pool memory.Allocator,
		schema *arrow.Schema,
		options IterOptions,
		callback func(ctx context.Context, r arrow.Record) error,
	) error
	SchemaIterator(
		ctx context.Context,
		tx uint64,
		pool memory.Allocator,
		options IterOptions,
		callback func(ctx context.Context, r arrow.Record) error,
	) error
	ArrowSchema(
		ctx context.Context,
		tx uint64,
		pool memory.Allocator,
		options IterOptions,
	) (*arrow.Schema, error)
	Schema() *dynparquet.Schema
}
type TableProvider interface {
	GetTable(name string) TableReader
}

type TableScan struct {
	TableProvider TableProvider
	TableName     string

	// PhysicalProjection describes the columns that are to be physically read
	// by the table scan.
	PhysicalProjection []Expr

	// Filter is the predicate that is to be applied by the table scan to rule
	// out any blocks of data to be scanned at all.
	Filter Expr

	// Distinct describes the columns that are to be distinct.
	Distinct []Expr

	// Projection is the list of columns that are to be projected.
	Projection []Expr
}

func (scan *TableScan) String() string {
	return "TableScan" +
		" Table: " + scan.TableName +
		" Projection: " + fmt.Sprint(scan.Projection) +
		" Filter: " + fmt.Sprint(scan.Filter) +
		" Distinct: " + fmt.Sprint(scan.Distinct)
}

type SchemaScan struct {
	TableProvider TableProvider
	TableName     string

	// PhysicalProjection describes the columns that are to be physically read
	// by the table scan.
	PhysicalProjection []Expr

	// filter is the predicate that is to be applied by the table scan to rule
	// out any blocks of data to be scanned at all.
	Filter Expr

	// Distinct describes the columns that are to be distinct.
	Distinct []Expr

	// Projection is the list of columns that are to be projected.
	Projection []Expr
}

func (s *SchemaScan) String() string {
	return "SchemaScan"
}

type Filter struct {
	Expr Expr
}

func (f *Filter) String() string {
	return "Filter" + " Expr: " + fmt.Sprint(f.Expr)
}

type Distinct struct {
	Exprs []Expr
}

func (d *Distinct) String() string {
	return "Distinct"
}

type Projection struct {
	Exprs []Expr
}

func (p *Projection) String() string {
	return "Projection"
}

type Aggregation struct {
	GroupExprs []Expr
	AggExpr    Expr
}

func (a *Aggregation) String() string {
	return "Aggregation " + fmt.Sprint(a.AggExpr) + " Group: " + fmt.Sprint(a.GroupExprs)
}
