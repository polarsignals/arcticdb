package builder

import (
	"reflect"
	"sync/atomic"
	"unsafe"

	"github.com/apache/arrow/go/v8/arrow"
	"github.com/apache/arrow/go/v8/arrow/array"
	"github.com/apache/arrow/go/v8/arrow/bitutil"
	"github.com/apache/arrow/go/v8/arrow/memory"
	"github.com/segmentio/parquet-go"
)

// ColumnBuilder is a subset of the array.Builder interface implemented by the
// optimized builders in this file.
type ColumnBuilder interface {
	Retain()
	Release()
	Len() int
	AppendNull()
	Reserve(int)
	NewArray() arrow.Array
}

// OptimizedBuilder is a set of FrostDB specific builder methods.
type OptimizedBuilder interface {
	ColumnBuilder
	AppendNulls(int)
	ResetToLength(int)
	RepeatLastValue(int)
}

type builderBase struct {
	dtype          arrow.DataType
	refCount       int64
	length         int
	validityBitmap []byte
}

func (b *builderBase) reset() {
	b.length = 0
	b.validityBitmap = b.validityBitmap[:0]
}

func (b *builderBase) Retain() {
	atomic.AddInt64(&b.refCount, 1)
}

func (b *builderBase) releaseInternal() {
	b.length = 0
	b.validityBitmap = nil
}

func (b *builderBase) Release() {
	atomic.AddInt64(&b.refCount, -1)
	b.releaseInternal()
}

// Len returns the number of elements in the array builder.
func (b *builderBase) Len() int {
	return b.length
}

func (b *builderBase) Reserve(int) {}

// AppendNulls appends n null values to the array being built. This is specific
// to distinct optimizations in FrostDB.
func (b *builderBase) AppendNulls(n int) {
	b.validityBitmap = resizeBitmap(b.validityBitmap, b.length+n)
	bitutil.SetBitsTo(b.validityBitmap, int64(b.length), int64(n), false)
	b.length += n
}

// appendValid does the opposite of appendNulls.
func (b *builderBase) appendValid(n int) {
	b.validityBitmap = resizeBitmap(b.validityBitmap, b.length+n)
	bitutil.SetBitsTo(b.validityBitmap, int64(b.length), int64(n), true)
	b.length += n
}

func resizeBitmap(bitmap []byte, valuesToRepresent int) []byte {
	bytesNeeded := int(bitutil.BytesForBits(int64(valuesToRepresent)))
	if cap(bitmap) < bytesNeeded {
		existingBitmap := bitmap
		bitmap = make([]byte, bitutil.NextPowerOf2(bytesNeeded))
		copy(bitmap, existingBitmap)
	}
	return bitmap[:bytesNeeded]
}

var (
	_ OptimizedBuilder = (*OptBinaryBuilder)(nil)
	_ OptimizedBuilder = (*OptInt64Builder)(nil)
	_ OptimizedBuilder = (*OptBooleanBuilder)(nil)
)

// OptBinaryBuilder is an optimized array.BinaryBuilder.
type OptBinaryBuilder struct {
	builderBase

	data []byte
	// offsets are offsets into data. The ith value is
	// data[offsets[i]:offsets[i+1]]. Note however, that during normal operation,
	// len(data) is never appended to the slice until the next value is added,
	// i.e. the last offset is never closed until the offsets slice is appended
	// to or returned to the caller.
	offsets []uint32
}

func NewOptBinaryBuilder(dtype arrow.BinaryDataType) *OptBinaryBuilder {
	b := &OptBinaryBuilder{}
	b.dtype = dtype
	return b
}

// Release decreases the reference count by 1.
// When the reference count goes to zero, the memory is freed.
// Release may be called simultaneously from multiple goroutines.
func (b *OptBinaryBuilder) Release() {
	if atomic.AddInt64(&b.refCount, -1) == 0 {
		b.data = nil
		b.offsets = nil
		b.releaseInternal()
	}
}

// AppendNull adds a new null value to the array being built. This is slow,
// don't use it.
func (b *OptBinaryBuilder) AppendNull() {
	b.offsets = append(b.offsets, uint32(len(b.data)))
	b.builderBase.AppendNulls(1)
}

// AppendNulls appends n null values to the array being built. This is specific
// to distinct optimizations in FrostDB.
func (b *OptBinaryBuilder) AppendNulls(n int) {
	for i := 0; i < n; i++ {
		b.offsets = append(b.offsets, uint32(len(b.data)))
	}
	b.builderBase.AppendNulls(n)
}

// NewArray creates a new array from the memory buffers used
// by the builder and resets the Builder so it can be used to build
// a new array.
func (b *OptBinaryBuilder) NewArray() arrow.Array {
	b.offsets = append(b.offsets, uint32(len(b.data)))
	var offsetsAsBytes []byte

	fromHeader := (*reflect.SliceHeader)(unsafe.Pointer(&b.offsets))
	toHeader := (*reflect.SliceHeader)(unsafe.Pointer(&offsetsAsBytes))
	toHeader.Data = fromHeader.Data
	toHeader.Len = fromHeader.Len * arrow.Uint32SizeBytes
	toHeader.Cap = fromHeader.Cap * arrow.Uint32SizeBytes

	data := array.NewData(
		b.dtype,
		b.length,
		[]*memory.Buffer{
			memory.NewBufferBytes(b.validityBitmap),
			memory.NewBufferBytes(offsetsAsBytes),
			memory.NewBufferBytes(b.data),
		},
		nil,
		b.length-bitutil.CountSetBits(b.validityBitmap, 0, b.length),
		0,
	)
	b.reset()
	b.offsets = b.offsets[:0]
	b.data = b.data[:0]
	return array.NewBinaryData(data)
}

// AppendData appends a flat slice of bytes to the builder, with an accompanying
// slice of offsets. This data is considered to be non-null.
func (b *OptBinaryBuilder) AppendData(data []byte, offsets []uint32) {
	// Trim the last offset since we want this last range to be "open".
	offsets = offsets[:len(offsets)-1]

	offsetConversion := uint32(len(b.data))
	b.data = append(b.data, data...)
	startOffset := len(b.offsets)
	b.offsets = append(b.offsets, offsets...)
	for curOffset := startOffset; curOffset < len(b.offsets); curOffset++ {
		b.offsets[curOffset] += offsetConversion
	}

	b.length += len(offsets)
	b.validityBitmap = resizeBitmap(b.validityBitmap, b.length)
	bitutil.SetBitsTo(b.validityBitmap, int64(startOffset), int64(len(offsets)), true)
}

func (b *OptBinaryBuilder) Append(v []byte) {
	b.offsets = append(b.offsets, uint32(len(b.data)))
	b.data = append(b.data, v...)
	b.length++
	b.validityBitmap = resizeBitmap(b.validityBitmap, b.length)
	bitutil.SetBit(b.validityBitmap, b.length-1)
}

// AppendParquetValues appends the given parquet values to the builder. The
// values may be null, but if it is known upfront that none of the values are
// null, AppendData offers a more efficient way of appending values.
func (b *OptBinaryBuilder) AppendParquetValues(values []parquet.Value) {
	for i := range values {
		b.offsets = append(b.offsets, uint32(len(b.data)))
		b.data = append(b.data, values[i].ByteArray()...)
	}

	oldLength := b.length
	b.length += len(values)

	b.validityBitmap = resizeBitmap(b.validityBitmap, b.length)
	for i := range values {
		bitutil.SetBitTo(b.validityBitmap, oldLength+i, !values[i].IsNull())
	}
}

// RepeatLastValue is specific to distinct optimizations in FrostDB.
func (b *OptBinaryBuilder) RepeatLastValue(n int) {
	if bitutil.BitIsNotSet(b.validityBitmap, b.length-1) {
		// Last value is null.
		b.AppendNulls(n)
		return
	}

	lastValue := b.data[b.offsets[len(b.offsets)-1]:]
	for i := 0; i < n; i++ {
		b.offsets = append(b.offsets, uint32(len(b.data)))
		b.data = append(b.data, lastValue...)
	}
	b.appendValid(n)
}

// ResetToLength is specific to distinct optimizations in FrostDB.
func (b *OptBinaryBuilder) ResetToLength(n int) {
	if n == b.length {
		return
	}

	b.length = n
	b.data = b.data[:b.offsets[n]]
	b.offsets = b.offsets[:n]
	b.validityBitmap = resizeBitmap(b.validityBitmap, n)
}

type OptInt64Builder struct {
	builderBase

	data []int64
}

func NewOptInt64Builder(dtype arrow.DataType) *OptInt64Builder {
	b := &OptInt64Builder{}
	b.dtype = dtype
	return b
}

func (b *OptInt64Builder) resizeData(neededLength int) {
	if cap(b.data) < neededLength {
		oldData := b.data
		b.data = make([]int64, bitutil.NextPowerOf2(neededLength))
		copy(b.data, oldData)
	}
	b.data = b.data[:neededLength]
}

func (b *OptInt64Builder) Release() {
	if atomic.AddInt64(&b.refCount, -1) == 0 {
		b.data = nil
		b.releaseInternal()
	}
}

func (b *OptInt64Builder) AppendNull() {
	b.AppendNulls(1)
}

func (b *OptInt64Builder) AppendNulls(n int) {
	b.resizeData(b.length + n)
	b.builderBase.AppendNulls(n)
}

func (b *OptInt64Builder) NewArray() arrow.Array {
	var dataAsBytes []byte

	fromHeader := (*reflect.SliceHeader)(unsafe.Pointer(&b.data))
	toHeader := (*reflect.SliceHeader)(unsafe.Pointer(&dataAsBytes))
	toHeader.Data = fromHeader.Data
	toHeader.Len = fromHeader.Len * arrow.Int64SizeBytes
	toHeader.Cap = fromHeader.Cap * arrow.Int64SizeBytes

	data := array.NewData(
		b.dtype,
		b.length,
		[]*memory.Buffer{
			memory.NewBufferBytes(b.validityBitmap),
			memory.NewBufferBytes(dataAsBytes),
		},
		nil,
		b.length-bitutil.CountSetBits(b.validityBitmap, 0, b.length),
		0,
	)
	b.reset()
	b.data = b.data[:0]
	return array.NewInt64Data(data)
}

// AppendData appends a slice of int64s to the builder. This data is considered
// to be non-null.
func (b *OptInt64Builder) AppendData(data []int64) {
	oldLength := b.length
	b.data = append(b.data, data...)
	b.length += len(data)
	b.validityBitmap = resizeBitmap(b.validityBitmap, b.length)
	bitutil.SetBitsTo(b.validityBitmap, int64(oldLength), int64(len(data)), true)
}

func (b *OptInt64Builder) Append(v int64) {
	b.data = append(b.data, v)
	b.length++
	b.validityBitmap = resizeBitmap(b.validityBitmap, b.length)
	bitutil.SetBit(b.validityBitmap, b.length-1)
}

func (b *OptInt64Builder) AppendParquetValues(values []parquet.Value) {
	b.resizeData(b.length + len(values))
	b.validityBitmap = resizeBitmap(b.validityBitmap, b.length+len(values))
	for i, j := b.length, 0; i < b.length+len(values) && j < len(values); {
		b.data[i] = values[j].Int64()
		bitutil.SetBitTo(b.validityBitmap, i, !values[j].IsNull())
		i++
		j++
	}
	b.length += len(values)
}

func (b *OptInt64Builder) RepeatLastValue(n int) {
	if bitutil.BitIsNotSet(b.validityBitmap, b.length-1) {
		b.AppendNulls(n)
		return
	}

	lastValue := b.data[b.length-1]
	b.resizeData(b.length + n)
	for i := b.length; i < b.length+n; i++ {
		b.data[i] = lastValue
	}
	b.appendValid(n)
}

// ResetToLength is specific to distinct optimizations in FrostDB.
func (b *OptInt64Builder) ResetToLength(n int) {
	if n == b.length {
		return
	}

	b.length = n
	b.data = b.data[:n]
	b.validityBitmap = resizeBitmap(b.validityBitmap, n)
}

type OptBooleanBuilder struct {
	builderBase
	data []byte
}

func NewOptBooleanBuilder(dtype arrow.DataType) *OptBooleanBuilder {
	b := &OptBooleanBuilder{}
	b.dtype = dtype
	return b
}

func (b *OptBooleanBuilder) Release() {
	if atomic.AddInt64(&b.refCount, -1) == 0 {
		b.data = nil
		b.releaseInternal()
	}
}

func (b *OptBooleanBuilder) AppendNull() {
	b.AppendNulls(1)
}

func (b *OptBooleanBuilder) AppendNulls(n int) {
	v := b.length + n
	b.data = resizeBitmap(b.data, v)
	b.validityBitmap = resizeBitmap(b.validityBitmap, v)

	for i := 0; i < n; i++ {
		bitutil.SetBitTo(b.data, b.length, false)
		bitutil.SetBitTo(b.validityBitmap, b.length, false)
		b.length++
	}
}

func (b *OptBooleanBuilder) NewArray() arrow.Array {
	data := array.NewData(
		b.dtype,
		b.length,
		[]*memory.Buffer{
			memory.NewBufferBytes(b.validityBitmap),
			memory.NewBufferBytes(b.data),
		},
		nil,
		b.length-bitutil.CountSetBits(b.validityBitmap, 0, b.length),
		0,
	)
	b.reset()
	b.data = b.data[:0]
	array := array.NewBooleanData(data)
	return array
}

func (b *OptBooleanBuilder) Append(data []byte, valid int) {
	n := b.length + valid
	b.data = resizeBitmap(b.data, n)
	b.validityBitmap = resizeBitmap(b.validityBitmap, n)

	// TODO: This isn't ideal setting bits 1 by 1, when we could copy in all the bits
	for i := 0; i < valid; i++ {
		bitutil.SetBitTo(b.data, b.length, bitutil.BitIsSet(data, i))
		bitutil.SetBitTo(b.validityBitmap, b.length, true)
		b.length++
	}
}

func (b *OptBooleanBuilder) AppendData(data []byte) {
	panic("do not use AppendData for opt boolean builder, use Append instead")
}

func (b *OptBooleanBuilder) AppendParquetValues(values []parquet.Value) {
	n := b.length + len(values)
	b.data = resizeBitmap(b.data, n)
	b.validityBitmap = resizeBitmap(b.validityBitmap, n)

	for _, v := range values {
		bitutil.SetBitTo(b.data, b.length, v.Boolean())
		bitutil.SetBitTo(b.validityBitmap, b.length, true)
		b.length++
	}
}

func (b *OptBooleanBuilder) AppendSingle(v bool) {
	b.length++
	b.data = resizeBitmap(b.data, b.length)
	b.validityBitmap = resizeBitmap(b.validityBitmap, b.length)
	bitutil.SetBitTo(b.data, b.length-1, v)
	bitutil.SetBit(b.validityBitmap, b.length-1)
}

func (b *OptBooleanBuilder) RepeatLastValue(n int) {
	if bitutil.BitIsNotSet(b.validityBitmap, b.length-1) {
		b.AppendNulls(n)
		return
	}

	lastValue := bitutil.BitIsSet(b.data, b.length-1)
	b.data = resizeBitmap(b.data, b.length+n)
	bitutil.SetBitsTo(b.data, int64(b.length), int64(n), lastValue)
	b.appendValid(n)
}

// ResetToLength is specific to distinct optimizations in FrostDB.
func (b *OptBooleanBuilder) ResetToLength(n int) {
	if n == b.length {
		return
	}

	b.length = n
	b.data = resizeBitmap(b.data, n)
	b.validityBitmap = resizeBitmap(b.validityBitmap, n)
}
