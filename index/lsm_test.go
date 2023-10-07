package index

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/apache/arrow/go/v14/arrow"
	"github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/require"

	"github.com/polarsignals/frostdb/dynparquet"
	"github.com/polarsignals/frostdb/parts"
)

func parquetCompaction(compact []*parts.Part) ([]*parts.Part, int64, int64, error) {
	b := &bytes.Buffer{}
	size, err := compactParts(b, compact)
	if err != nil {
		return nil, 0, 0, err
	}

	buf, err := dynparquet.ReaderFromBytes(b.Bytes())
	if err != nil {
		return nil, 0, 0, err
	}
	return []*parts.Part{parts.NewPart(0, buf)}, size, int64(b.Len()), nil
}

func compactParts(w io.Writer, compact []*parts.Part) (int64, error) {
	schema := dynparquet.NewSampleSchema()
	bufs := []dynparquet.DynamicRowGroup{}
	var size int64
	for _, part := range compact {
		size += part.Size()
		buf, err := part.AsSerializedBuffer(schema)
		if err != nil {
			return 0, err
		}
		bufs = append(bufs, buf.MultiDynamicRowGroup())
	}
	merged, err := schema.MergeDynamicRowGroups(bufs)
	if err != nil {
		return 0, err
	}
	err = func() error {
		writer, err := schema.GetWriter(w, merged.DynamicColumns())
		if err != nil {
			return err
		}
		defer writer.Close()

		rows := merged.Rows()
		defer rows.Close()

		buf := make([]parquet.Row, merged.NumRows())
		if _, err := rows.ReadRows(buf); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if _, err := writer.WriteRows(buf); err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		return nil
	}()
	if err != nil {
		return 0, err
	}

	return size, nil
}

func check(t *testing.T, lsm *LSM, records, buffers int) {
	t.Helper()
	seen := map[SentinelType]bool{}
	lsm.levels.Iterate(func(node *Node) bool {
		if node.part == nil {
			if seen[node.sentinel] {
				t.Fatal("duplicate sentinel")
			}
			seen[node.sentinel] = true
		}
		return true
	})
	rec := 0
	buf := 0
	require.NoError(t, lsm.Scan(context.Background(), "", nil, nil, 2, func(ctx context.Context, v any) error {
		switch v.(type) {
		case arrow.Record:
			rec++
		case dynparquet.DynamicRowGroup:
			buf++
		}
		return nil
	}))
	require.Equal(t, records, rec)
	require.Equal(t, buf, buffers)
}

func Test_LSM_Basic(t *testing.T) {
	t.Parallel()
	lsm, err := NewLSM("test", []*LevelConfig{
		{Level: L0, MaxSize: 1024 * 1024 * 1024, Compact: parquetCompaction},
		{Level: L1, MaxSize: 1024 * 1024 * 1024, Compact: parquetCompaction},
		{Level: L2, MaxSize: 1024 * 1024 * 1024},
	})
	require.NoError(t, err)

	samples := dynparquet.NewTestSamples()
	r, err := samples.ToRecord()
	require.NoError(t, err)

	lsm.Add(1, r)
	lsm.Add(1, r)
	lsm.Add(1, r)
	check(t, lsm, 3, 0)
	require.NoError(t, lsm.merge(L0, nil))
	check(t, lsm, 0, 1)
	lsm.Add(1, r)
	check(t, lsm, 1, 1)
	lsm.Add(1, r)
	check(t, lsm, 2, 1)
	require.NoError(t, lsm.merge(L0, nil))
	check(t, lsm, 0, 2)
	lsm.Add(1, r)
	check(t, lsm, 1, 2)
	require.NoError(t, lsm.merge(L1, nil))
	check(t, lsm, 1, 1)
	require.NoError(t, lsm.merge(L0, nil))
	check(t, lsm, 0, 2)
}

func Test_LSM_DuplicateSentinel(t *testing.T) {
	t.Parallel()
	lsm, err := NewLSM("test", []*LevelConfig{
		{Level: L0, MaxSize: 1024 * 1024 * 1024, Compact: parquetCompaction},
		{Level: L1, MaxSize: 1024 * 1024 * 1024, Compact: parquetCompaction},
		{Level: L2, MaxSize: 1024 * 1024 * 1024},
	})
	require.NoError(t, err)

	samples := dynparquet.NewTestSamples()
	r, err := samples.ToRecord()
	require.NoError(t, err)

	lsm.Add(1, r)
	lsm.Add(1, r)
	lsm.Add(1, r)
	check(t, lsm, 3, 0)
	require.NoError(t, lsm.merge(L0, nil))
	check(t, lsm, 0, 1)
	require.NoError(t, lsm.merge(L0, nil))
	check(t, lsm, 0, 1)
}

func Test_LSM_Compaction(t *testing.T) {
	t.Parallel()
	lsm, err := NewLSM("test", []*LevelConfig{
		{Level: L0, MaxSize: 1, Compact: parquetCompaction},
		{Level: L1, MaxSize: 1024 * 1024 * 1024},
	})
	require.NoError(t, err)

	samples := dynparquet.NewTestSamples()
	r, err := samples.ToRecord()
	require.NoError(t, err)

	lsm.Add(1, r)
	require.Eventually(t, func() bool {
		return lsm.sizes[L0].Load() == 0 && lsm.sizes[L1].Load() != 0
	}, time.Second, 5*time.Millisecond)
}

func Test_LSM_CascadeCompaction(t *testing.T) {
	t.Parallel()
	lsm, err := NewLSM("test", []*LevelConfig{
		{Level: L0, MaxSize: 257, Compact: parquetCompaction},
		{Level: L1, MaxSize: 2281, Compact: parquetCompaction},
		{Level: L2, MaxSize: 2281, Compact: parquetCompaction},
		{Level: 3, MaxSize: 2281, Compact: parquetCompaction},
		{Level: 4, MaxSize: 2281},
	})
	require.NoError(t, err)

	samples := dynparquet.NewTestSamples()
	r, err := samples.ToRecord()
	require.NoError(t, err)

	lsm.Add(1, r)
	require.Eventually(t, func() bool {
		return lsm.sizes[L0].Load() == 0 &&
			lsm.sizes[L1].Load() != 0 &&
			lsm.sizes[L2].Load() == 0 &&
			lsm.sizes[3].Load() == 0 &&
			lsm.sizes[4].Load() == 0
	}, time.Second, 5*time.Millisecond)
	lsm.Add(1, r)
	require.Eventually(t, func() bool {
		return lsm.sizes[L0].Load() == 0 &&
			lsm.sizes[L1].Load() == 0 &&
			lsm.sizes[L2].Load() == 0 &&
			lsm.sizes[3].Load() == 0 &&
			lsm.sizes[4].Load() != 0
	}, time.Second, 5*time.Millisecond)
}