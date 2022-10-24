package dynparquet

import (
	"github.com/segmentio/parquet-go"
)

type DynamicRows struct {
	Rows           []parquet.Row
	Schema         *parquet.Schema
	DynamicColumns map[string][]string
	fields         []parquet.Field
}

func (r *DynamicRows) Get(i int) *DynamicRow {
	return &DynamicRow{
		Schema:         r.Schema,
		DynamicColumns: r.DynamicColumns,
		Row:            r.Rows[i],
		fields:         r.fields,
	}
}

func (r *DynamicRows) GetCopy(i int) *DynamicRow {
	rowCopy := make(parquet.Row, len(r.Rows[i]))
	for i, v := range r.Rows[i] {
		rowCopy[i] = v.Clone()
	}

	return &DynamicRow{
		Schema:         r.Schema,
		DynamicColumns: r.DynamicColumns,
		Row:            rowCopy,
		fields:         r.fields,
	}
}

type DynamicRow struct {
	Row            parquet.Row
	Schema         *parquet.Schema
	DynamicColumns map[string][]string
	fields         []parquet.Field
}

func (s *Schema) RowLessThan(a, b *DynamicRow) bool {
	return s.Cmp(a, b) < 0
}

func (s *Schema) Cmp(a, b *DynamicRow) int {
	dynamicColumns := mergeDynamicColumnSets([]map[string][]string{a.DynamicColumns, b.DynamicColumns})
	cols := s.parquetSortingColumns(dynamicColumns)
	for _, col := range cols {
		name := col.Path()[0] // Currently we only support flat schemas.

		aIndex := FindChildIndex(a.fields, name)
		bIndex := FindChildIndex(b.fields, name)

		if aIndex == -1 && bIndex == -1 {
			continue
		}

		var node parquet.Node
		if aIndex != -1 {
			node = FieldByName(a.fields, name)
		} else {
			node = FieldByName(b.fields, name)
		}

		av, bv := extractValues(a, b, aIndex, bIndex)
		cmp := compare(col, node, av, bv)
		if cmp < 0 {
			return cmp
		}
		if cmp > 0 {
			return cmp
		}
		// neither of those case are true so a and b are equal for this column
		// and we need to continue with the next column.
	}

	return 0
}

func compare(col parquet.SortingColumn, node parquet.Node, av, bv []parquet.Value) int {
	sortOptions := []parquet.SortOption{
		parquet.SortDescending(col.Descending()),
		parquet.SortNullsFirst(col.NullsFirst()),
	}
	if node.Optional() || node.Repeated() {
		sortOptions = append(sortOptions, parquet.SortMaxDefinitionLevel(1))
	}

	if node.Repeated() {
		sortOptions = append(sortOptions, parquet.SortMaxRepetitionLevel(1))
	}

	return parquet.SortFuncOf(
		node.Type(),
		sortOptions...,
	)(av, bv)
}

func extractValues(a, b *DynamicRow, aIndex, bIndex int) ([]parquet.Value, []parquet.Value) {
	if aIndex != -1 && bIndex == -1 {
		return ValuesForIndex(a.Row, aIndex), []parquet.Value{parquet.ValueOf(nil).Level(0, 0, aIndex)}
	}

	if aIndex == -1 && bIndex != -1 {
		return []parquet.Value{parquet.ValueOf(nil).Level(0, 0, bIndex)}, ValuesForIndex(b.Row, bIndex)
	}

	return ValuesForIndex(a.Row, aIndex), ValuesForIndex(b.Row, bIndex)
}

func FindChildIndex(fields []parquet.Field, name string) int {
	for i, field := range fields {
		if field.Name() == name {
			return i
		}
	}
	return -1
}

func ValuesForIndex(row parquet.Row, index int) []parquet.Value {
	start := -1
	end := -1
	for i, v := range row {
		idx := v.Column()
		if end != -1 && end == idx-1 {
			return row[start:end]
		}
		if idx == index {
			if start == -1 {
				start = i
			}
			end = i + 1
		}
	}

	return row[start:end]
}
