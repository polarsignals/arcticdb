package physicalplan

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/apache/arrow/go/v14/arrow"
	"github.com/apache/arrow/go/v14/arrow/array"
	"github.com/apache/arrow/go/v14/arrow/scalar"

	"github.com/polarsignals/frostdb/query/logicalplan"
)

type ArrayRef struct {
	ColumnName string
}

func (a *ArrayRef) ArrowArray(r arrow.Record) (arrow.Array, bool, error) {
	fields := r.Schema().FieldIndices(a.ColumnName)
	if len(fields) != 1 {
		return nil, false, nil
	}

	return r.Column(fields[0]), true, nil
}

func (a *ArrayRef) String() string {
	return a.ColumnName
}

type BinaryScalarExpr struct {
	Left  *ArrayRef
	Op    logicalplan.Op
	Right scalar.Scalar
}

func (e BinaryScalarExpr) Eval(r arrow.Record) (*Bitmap, error) {
	leftData, exists, err := e.Left.ArrowArray(r)
	if err != nil {
		return nil, err
	}

	if !exists {
		res := NewBitmap()
		switch e.Op {
		case logicalplan.OpEq:
			if e.Right.IsValid() { // missing column; looking for == non-nil
				switch t := e.Right.(type) {
				case *scalar.Binary:
					if t.String() != "" { // treat empty string equivalent to nil
						return res, nil
					}
				case *scalar.String:
					if t.String() != "" { // treat empty string equivalent to nil
						return res, nil
					}
				}
			}
		case logicalplan.OpNotEq: // missing column; looking for != nil
			if !e.Right.IsValid() {
				return res, nil
			}
		case logicalplan.OpLt, logicalplan.OpLtEq, logicalplan.OpGt, logicalplan.OpGtEq:
			return res, nil
		}

		res.AddRange(0, uint64(r.NumRows()))
		return res, nil
	}

	return BinaryScalarOperation(leftData, e.Right, e.Op)
}

func (e BinaryScalarExpr) String() string {
	return e.Left.String() + " " + e.Op.String() + " " + e.Right.String()
}

var ErrUnsupportedBinaryOperation = errors.New("unsupported binary operation")

func BinaryScalarOperation(left arrow.Array, right scalar.Scalar, operator logicalplan.Op) (*Bitmap, error) {
	leftType := left.DataType()
	switch leftType {
	case arrow.FixedWidthTypes.Boolean:
		switch operator {
		case logicalplan.OpEq:
			return ArrayScalarEqual(left, right)
		case logicalplan.OpNotEq:
			return BooleanArrayScalarNotEqual(left.(*array.Boolean), right.(*scalar.Boolean))
		default:
			panic("something terrible has happened, this should have errored previously during validation")
		}
	case &arrow.FixedSizeBinaryType{ByteWidth: 16}:
		switch operator {
		case logicalplan.OpEq:
			return ArrayScalarEqual(left, right)
		case logicalplan.OpNotEq:
			return FixedSizeBinaryArrayScalarNotEqual(left.(*array.FixedSizeBinary), right.(*scalar.FixedSizeBinary))
		default:
			panic("something terrible has happened, this should have errored previously during validation")
		}
	case arrow.BinaryTypes.String:
		switch operator {
		case logicalplan.OpEq:
			return ArrayScalarEqual(left, right)
		case logicalplan.OpNotEq:
			return StringArrayScalarNotEqual(left.(*array.String), right.(*scalar.String))
		default:
			panic("something terrible has happened, this should have errored previously during validation")
		}
	case arrow.BinaryTypes.Binary:
		switch operator {
		case logicalplan.OpEq:
			switch r := right.(type) {
			case *scalar.Binary:
				return ArrayScalarEqual(left, r)
			case *scalar.String:
				return ArrayScalarEqual(left, r.Binary)
			default:
				panic("something terrible has happened, this should have errored previously during validation")
			}
		case logicalplan.OpNotEq:
			switch r := right.(type) {
			case *scalar.Binary:
				return BinaryArrayScalarNotEqual(left.(*array.Binary), r)
			case *scalar.String:
				return BinaryArrayScalarNotEqual(left.(*array.Binary), r.Binary)
			default:
				panic("something terrible has happened, this should have errored previously during validation")
			}
		default:
			panic("something terrible has happened, this should have errored previously during validation")
		}
	case arrow.PrimitiveTypes.Int64:
		switch operator {
		case logicalplan.OpEq:
			return ArrayScalarEqual(left, right)
		case logicalplan.OpNotEq:
			return Int64ArrayScalarNotEqual(left.(*array.Int64), right.(*scalar.Int64))
		case logicalplan.OpLt:
			return Int64ArrayScalarLessThan(left.(*array.Int64), right.(*scalar.Int64))
		case logicalplan.OpLtEq:
			return Int64ArrayScalarLessThanOrEqual(left.(*array.Int64), right.(*scalar.Int64))
		case logicalplan.OpGt:
			return Int64ArrayScalarGreaterThan(left.(*array.Int64), right.(*scalar.Int64))
		case logicalplan.OpGtEq:
			return Int64ArrayScalarGreaterThanOrEqual(left.(*array.Int64), right.(*scalar.Int64))
		default:
			panic("something terrible has happened, this should have errored previously during validation")
		}
	case arrow.PrimitiveTypes.Int32:
		switch operator {
		case logicalplan.OpEq:
			return ArrayScalarEqual(left, right)
		case logicalplan.OpNotEq:
			return Int32ArrayScalarNotEqual(left.(*array.Int32), right.(*scalar.Int32))
		case logicalplan.OpLt:
			return Int32ArrayScalarLessThan(left.(*array.Int32), right.(*scalar.Int32))
		case logicalplan.OpLtEq:
			return Int32ArrayScalarLessThanOrEqual(left.(*array.Int32), right.(*scalar.Int32))
		case logicalplan.OpGt:
			return Int32ArrayScalarGreaterThan(left.(*array.Int32), right.(*scalar.Int32))
		case logicalplan.OpGtEq:
			return Int32ArrayScalarGreaterThanOrEqual(left.(*array.Int32), right.(*scalar.Int32))
		default:
			panic("something terrible has happened, this should have errored previously during validation")
		}
	case arrow.PrimitiveTypes.Uint64:
		switch operator {
		case logicalplan.OpEq:
			return ArrayScalarEqual(left, right)
		case logicalplan.OpNotEq:
			return Uint64ArrayScalarNotEqual(left.(*array.Uint64), right.(*scalar.Uint64))
		case logicalplan.OpLt:
			return Uint64ArrayScalarLessThan(left.(*array.Uint64), right.(*scalar.Uint64))
		case logicalplan.OpLtEq:
			return Uint64ArrayScalarLessThanOrEqual(left.(*array.Uint64), right.(*scalar.Uint64))
		case logicalplan.OpGt:
			return Uint64ArrayScalarGreaterThan(left.(*array.Uint64), right.(*scalar.Uint64))
		case logicalplan.OpGtEq:
			return Uint64ArrayScalarGreaterThanOrEqual(left.(*array.Uint64), right.(*scalar.Uint64))
		default:
			panic("something terrible has happened, this should have errored previously during validation")
		}
	}

	switch arr := left.(type) {
	case *array.Dictionary:
		switch operator {
		case logicalplan.OpEq:
			return DictionaryArrayScalarEqual(arr, right)
		case logicalplan.OpNotEq:
			return DictionaryArrayScalarNotEqual(arr, right)
		default:
			return nil, fmt.Errorf("unsupported operator: %v", operator)
		}
	}

	switch leftType.(type) {
	case *arrow.ListType:
		panic("TODO: list comparisons unimplemented")
	}

	return nil, ErrUnsupportedBinaryOperation
}

func DictionaryArrayScalarNotEqual(left *array.Dictionary, right scalar.Scalar) (*Bitmap, error) {
	res := NewBitmap()
	var data []byte
	switch r := right.(type) {
	case *scalar.Binary:
		data = r.Data()
	case *scalar.String:
		data = r.Data()
	}

	// This is a special case for where the left side should not equal NULL
	if right == scalar.ScalarNull {
		for i := 0; i < left.Len(); i++ {
			if !left.IsNull(i) {
				res.Add(uint32(i))
			}
		}
		return res, nil
	}

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}

		switch dict := left.Dictionary().(type) {
		case *array.Binary:
			if !bytes.Equal(dict.Value(left.GetValueIndex(i)), data) {
				res.Add(uint32(i))
			}
		case *array.String:
			if dict.Value(left.GetValueIndex(i)) != string(data) {
				res.Add(uint32(i))
			}
		}
	}

	return res, nil
}

func DictionaryArrayScalarEqual(left *array.Dictionary, right scalar.Scalar) (*Bitmap, error) {
	res := NewBitmap()
	var data []byte
	switch r := right.(type) {
	case *scalar.Binary:
		data = r.Data()
	case *scalar.String:
		data = r.Data()
	}

	// This is a special case for where the left side should equal NULL
	if right == scalar.ScalarNull {
		for i := 0; i < left.Len(); i++ {
			if left.IsNull(i) {
				res.Add(uint32(i))
			}
		}
		return res, nil
	}

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}

		switch dict := left.Dictionary().(type) {
		case *array.Binary:
			if bytes.Equal(dict.Value(left.GetValueIndex(i)), data) {
				res.Add(uint32(i))
			}
		case *array.String:
			if dict.Value(left.GetValueIndex(i)) == string(data) {
				res.Add(uint32(i))
			}
		}
	}

	return res, nil
}

func FixedSizeBinaryArrayScalarNotEqual(left *array.FixedSizeBinary, right *scalar.FixedSizeBinary) (*Bitmap, error) {
	res := NewBitmap()
	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			res.Add(uint32(i))
			continue
		}
		if !bytes.Equal(left.Value(i), right.Data()) {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func StringArrayScalarNotEqual(left *array.String, right *scalar.String) (*Bitmap, error) {
	res := NewBitmap()
	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			res.Add(uint32(i))
			continue
		}
		if left.Value(i) != string(right.Data()) {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func BinaryArrayScalarNotEqual(left *array.Binary, right *scalar.Binary) (*Bitmap, error) {
	res := NewBitmap()
	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			res.Add(uint32(i))
			continue
		}
		if !bytes.Equal(left.Value(i), right.Data()) {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Int64ArrayScalarNotEqual(left *array.Int64, right *scalar.Int64) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			res.Add(uint32(i))
			continue
		}
		if left.Value(i) != right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Int64ArrayScalarLessThan(left *array.Int64, right *scalar.Int64) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) < right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Int64ArrayScalarLessThanOrEqual(left *array.Int64, right *scalar.Int64) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) <= right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Int64ArrayScalarGreaterThan(left *array.Int64, right *scalar.Int64) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) > right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Int64ArrayScalarGreaterThanOrEqual(left *array.Int64, right *scalar.Int64) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) >= right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func ArrayScalarEqual(left arrow.Array, right scalar.Scalar) (*Bitmap, error) {
	res := NewBitmap()
	switch arr := left.(type) {
	case *array.Boolean:
		rightType, ok := right.(*scalar.Boolean)
		if !ok {
			return nil, fmt.Errorf("expected scalar.Boolean, got %T", right)
		}
		for i := 0; i < arr.Len(); i++ {
			if arr.IsNull(i) {
				continue
			}
			if arr.Value(i) == rightType.Value {
				res.Add(uint32(i))
			}
		}
	case *array.FixedSizeBinary:
		rightType, ok := right.(*scalar.FixedSizeBinary)
		if !ok {
			return nil, fmt.Errorf("expected scalar.FixedSizeBinary, got %T", right)
		}
		for i := 0; i < arr.Len(); i++ {
			if arr.IsNull(i) {
				continue
			}
			if bytes.Equal(arr.Value(i), rightType.Data()) {
				res.Add(uint32(i))
			}
		}
	case *array.Int32:
		rightType, ok := right.(*scalar.Int32)
		if !ok {
			return nil, fmt.Errorf("expected scalar.Int32, got %T", right)
		}
		for i := 0; i < arr.Len(); i++ {
			if arr.IsNull(i) {
				continue
			}
			if arr.Value(i) == rightType.Value {
				res.Add(uint32(i))
			}
		}
	case *array.Int64:
		rightType, ok := right.(*scalar.Int64)
		if !ok {
			return nil, fmt.Errorf("expected scalar.Int64, got %T", right)
		}
		for i := 0; i < arr.Len(); i++ {
			if arr.IsNull(i) {
				continue
			}
			if arr.Value(i) == rightType.Value {
				res.Add(uint32(i))
			}
		}
	case *array.Uint64:
		rightType, ok := right.(*scalar.Uint64)
		if !ok {
			return nil, fmt.Errorf("expected scalar.Uint64, got %T", right)
		}
		for i := 0; i < arr.Len(); i++ {
			if arr.IsNull(i) {
				continue
			}
			if arr.Value(i) == rightType.Value {
				res.Add(uint32(i))
			}
		}
	default:
		return nil, fmt.Errorf("unsupported array type: %T", arr)
	}

	return res, nil
}

func Uint64ArrayScalarNotEqual(left *array.Uint64, right *scalar.Uint64) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			res.Add(uint32(i))
			continue
		}
		if left.Value(i) != right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Uint64ArrayScalarLessThan(left *array.Uint64, right *scalar.Uint64) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) < right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Uint64ArrayScalarLessThanOrEqual(left *array.Uint64, right *scalar.Uint64) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) <= right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Uint64ArrayScalarGreaterThan(left *array.Uint64, right *scalar.Uint64) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) > right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Uint64ArrayScalarGreaterThanOrEqual(left *array.Uint64, right *scalar.Uint64) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) >= right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func BooleanArrayScalarNotEqual(left *array.Boolean, right *scalar.Boolean) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) != right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Int32ArrayScalarNotEqual(left *array.Int32, right *scalar.Int32) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			res.Add(uint32(i))
			continue
		}
		if left.Value(i) != right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Int32ArrayScalarLessThan(left *array.Int32, right *scalar.Int32) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) < right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Int32ArrayScalarLessThanOrEqual(left *array.Int32, right *scalar.Int32) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) <= right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Int32ArrayScalarGreaterThan(left *array.Int32, right *scalar.Int32) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) > right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}

func Int32ArrayScalarGreaterThanOrEqual(left *array.Int32, right *scalar.Int32) (*Bitmap, error) {
	res := NewBitmap()

	for i := 0; i < left.Len(); i++ {
		if left.IsNull(i) {
			continue
		}
		if left.Value(i) >= right.Value {
			res.Add(uint32(i))
		}
	}

	return res, nil
}
