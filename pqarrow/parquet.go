package pqarrow

import (
	"fmt"
	"io"
	"strings"

	"github.com/apache/arrow/go/v10/arrow"
	"github.com/apache/arrow/go/v10/arrow/array"
	"github.com/apache/arrow/go/v10/arrow/scalar"
	"github.com/segmentio/parquet-go"

	"github.com/polarsignals/frostdb/bufutils"
	"github.com/polarsignals/frostdb/dynparquet"
)

func ArrowScalarToParquetValue(sc scalar.Scalar) (parquet.Value, error) {
	switch s := sc.(type) {
	case *scalar.String:
		return parquet.ValueOf(string(s.Data())), nil
	case *scalar.Int64:
		return parquet.ValueOf(s.Value), nil
	case *scalar.FixedSizeBinary:
		width := s.Type.(*arrow.FixedSizeBinaryType).ByteWidth
		v := [16]byte{}
		copy(v[:width], s.Data())
		return parquet.ValueOf(v), nil
	case *scalar.Boolean:
		return parquet.ValueOf(s.Value), nil
	case *scalar.Null:
		return parquet.NullValue(), nil
	case nil:
		return parquet.Value{}, nil
	default:
		return parquet.Value{}, fmt.Errorf("unsupported scalar type %T", s)
	}
}

func appendToRow(row []parquet.Value, c arrow.Array, index, rep, def, col int) ([]parquet.Value, error) {
	switch arr := c.(type) {
	case *array.Int64:
		row = append(row, parquet.ValueOf(arr.Value(index)).Level(rep, def, col))
	case *array.Boolean:
		row = append(row, parquet.ValueOf(arr.Value(index)).Level(rep, def, col))
	case *array.Binary:
		row = append(row, parquet.ValueOf(arr.Value(index)).Level(rep, def, col))
	case *array.String:
		row = append(row, parquet.ValueOf(arr.Value(index)).Level(rep, def, col))
	case *array.Uint64:
		row = append(row, parquet.ValueOf(arr.Value(index)).Level(rep, def, col))
	case *array.Dictionary:
		switch dict := arr.Dictionary().(type) {
		case *array.Binary:
			row = append(row, parquet.ValueOf(dict.Value(arr.GetValueIndex(index))).Level(rep, def, col))
		case *array.String:
			row = append(row, parquet.ValueOf(dict.Value(arr.GetValueIndex(index))).Level(rep, def, col))
		default:
			return nil, fmt.Errorf("dictionary not of expected type: %T", dict)
		}
	default:
		return nil, fmt.Errorf("column not of expected type: %v", c.DataType().ID())
	}

	return row, nil
}

// RecordToRow converts an arrow record with dynamic columns into a row using a dynamic parquet schema.
func RecordToRow(schema *dynparquet.Schema, final *parquet.Schema, record arrow.Record, index int) (parquet.Row, error) {
	return getRecordRow(schema, final, record, index, final.Fields(), record.Schema().Fields())
}

func getRecordRow(schema *dynparquet.Schema, final *parquet.Schema, record arrow.Record, index int, finalFields []parquet.Field, recordFields []arrow.Field) (parquet.Row, error) {
	var err error
	row := make([]parquet.Value, 0, len(finalFields))
	for i, f := range finalFields { // assuming flat schema
		found := false
		for j, af := range recordFields {
			if f.Name() == af.Name {
				def := 0
				if isDynamicColumn(schema, af.Name) {
					def = 1
				}
				row, err = appendToRow(row, record.Column(j), index, 0, def, i)
				if err != nil {
					return nil, err
				}
				found = true
				break
			}
		}

		// No record field found; append null
		if !found {
			row = append(row, parquet.ValueOf(nil).Level(0, 0, i))
		}
	}

	return row, nil
}

func isDynamicColumn(schema *dynparquet.Schema, column string) bool {
	parts := strings.SplitN(column, ".", 2)
	return len(parts) == 2 && schema.IsDynamicColumn(parts[0]) // dynamic column
}

func RecordDynamicCols(record arrow.Record) map[string][]string {
	dyncols := map[string][]string{}
	for _, af := range record.Schema().Fields() {
		parts := strings.SplitN(af.Name, ".", 2)
		if len(parts) == 2 { // dynamic column
			dyncols[parts[0]] = append(dyncols[parts[0]], parts[1])
		}
	}

	return bufutils.Dedupe(dyncols)
}

// RecordToDynamicSchema converts an arrow record into a parquet schema, dynamic cols, and parquet fields.
func RecordToDynamicSchema(schema *dynparquet.Schema, record arrow.Record) (*parquet.Schema, map[string][]string, []parquet.Field) {
	dyncols := map[string][]string{}
	g := parquet.Group{}
	for _, f := range schema.ParquetSchema().Fields() {
		for _, af := range record.Schema().Fields() {
			name := af.Name
			parts := strings.SplitN(name, ".", 2)
			if len(parts) == 2 { // dynamic column
				name = parts[0] // dedupe
				dyncols[parts[0]] = append(dyncols[parts[0]], parts[1])
			}

			if f.Name() == name {
				g[af.Name] = f
			}
		}
	}

	sc := parquet.NewSchema("arrow converted", g)
	return sc, bufutils.Dedupe(dyncols), sc.Fields()
}

func RecordToDynamicRow(schema *dynparquet.Schema, record arrow.Record, index int) (*dynparquet.DynamicRow, error) {
	if index >= int(record.NumRows()) {
		return nil, io.EOF
	}

	ps, err := schema.DynamicParquetSchema(RecordDynamicCols(record))
	if err != nil {
		return nil, err
	}

	row, err := RecordToRow(schema, ps, record, index)
	if err != nil {
		return nil, err
	}

	sch, dyncols, fields := RecordToDynamicSchema(schema, record)
	return dynparquet.NewDynamicRow(row, sch, dyncols, fields), nil
}

func RecordToFile(schema *dynparquet.Schema, w *parquet.GenericWriter[any], r arrow.Record) error {
	defer w.Close()

	ps, err := schema.DynamicParquetSchema(RecordDynamicCols(r))
	if err != nil {
		return err
	}

	rows := make([]parquet.Row, 0, r.NumRows())
	finalFields := ps.Fields()
	recordFields := r.Schema().Fields()
	for i := 0; i < int(r.NumRows()); i++ {
		row, err := getRecordRow(schema, ps, r, i, finalFields, recordFields)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}

	_, err = w.WriteRows(rows)
	if err != nil {
		return err
	}

	return nil
}
