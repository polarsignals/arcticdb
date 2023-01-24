package wal

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	walpb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/wal/v1alpha1"
)

func TestWAL(t *testing.T) {
	dir, err := os.MkdirTemp("", "frostdb-wal-test")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := Open(
		log.NewNopLogger(),
		prometheus.NewRegistry(),
		dir,
	)
	require.NoError(t, err)
	go w.Run(ctx)

	require.NoError(t, w.Log(1, &walpb.Record{
		Entry: &walpb.Entry{
			EntryType: &walpb.Entry_Write_{
				Write: &walpb.Entry_Write{
					Data:      []byte("test-data"),
					TableName: "test-table",
				},
			},
		},
	}))

	require.NoError(t, w.Close())

	w, err = Open(
		log.NewNopLogger(),
		prometheus.NewRegistry(),
		dir,
	)
	require.NoError(t, err)
	go w.Run(ctx)

	err = w.Replay(func(tx uint64, r *walpb.Record) error {
		require.Equal(t, uint64(1), tx)
		require.Equal(t, []byte("test-data"), r.Entry.GetWrite().Data)
		require.Equal(t, "test-table", r.Entry.GetWrite().TableName)
		return nil
	})
	require.NoError(t, err)

	require.NoError(t, w.Log(2, &walpb.Record{
		Entry: &walpb.Entry{
			EntryType: &walpb.Entry_Write_{
				Write: &walpb.Entry_Write{
					Data:      []byte("test-data-2"),
					TableName: "test-table",
				},
			},
		},
	}))

	require.NoError(t, w.Close())

	// Test after removing wal dir.
	require.NoError(t, os.RemoveAll(dir))
	w, err = Open(
		log.NewNopLogger(),
		prometheus.NewRegistry(),
		dir,
	)
	require.NoError(t, err)
	go w.Run(ctx)

	err = w.Replay(func(tx uint64, r *walpb.Record) error {
		return nil
	})
	require.NoError(t, err)
}

func TestCorruptWAL(t *testing.T) {
	path := t.TempDir()

	w, err := Open(
		log.NewNopLogger(),
		prometheus.NewRegistry(),
		path,
	)
	require.NoError(t, err)

	require.NoError(t, w.Log(1, &walpb.Record{
		Entry: &walpb.Entry{
			EntryType: &walpb.Entry_Write_{
				Write: &walpb.Entry_Write{
					Data:      []byte("test-data"),
					TableName: "test-table",
				},
			},
		},
	}))
	require.NoError(t, w.Close())

	corruptFilePath := filepath.Join(path, "00000000000000003466")
	// Write a file that has the correct WAL index format with some garbage.
	require.NoError(t, os.WriteFile(corruptFilePath, []byte("garbage"), 0o644))

	w, err = Open(
		log.NewNopLogger(),
		prometheus.NewRegistry(),
		path,
	)
	require.NoError(t, err)

	lastIdx, err := w.LastIndex()
	require.NoError(t, err)
	require.Equal(t, uint64(1), lastIdx)
}
