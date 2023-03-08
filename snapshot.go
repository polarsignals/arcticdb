package frostdb

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/apache/arrow/go/v10/arrow/ipc"
	"github.com/go-kit/log/level"
	"github.com/google/btree"
	"github.com/segmentio/parquet-go"
	"google.golang.org/protobuf/proto"

	"github.com/polarsignals/frostdb/dynparquet"
	schemapb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/schema/v1alpha1"
	schemav2pb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/schema/v1alpha2"
	snapshotpb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/snapshot/v1alpha1"
	"github.com/polarsignals/frostdb/parts"
)

// This file implements writing and reading database snapshots from disk.
// The snapshot format at the time of writing is as follows:
// 4-byte magic "FDBS"
// <Table 1 Granule 1 Part 1>
// <Table 2 Granule 1 Part 1>
// <Table 2 Granule 1 Part 2>
// <Table 2 Granule 2 Part 1>
// ...
// Footer/File Metadata
// 4-byte length in bytes of footer/file metadata (little endian)
// 4-byte version number (little endian)
// 4-byte checksum (little endian)
// 4-byte magic "FDBS"
//
// Readers should start reading a snapshot by first verifying that the magic
// bytes are correct, followed by the version number to ensure that the snapshot
// was encoded using a version the reader supports. A version bump could, for
// example, add compression to the data bytes of the file.
// Refer to minVersionSupported/maxVersionSupported for more details.

const (
	snapshotMagic = "FDBS"
	dirPerms      = os.FileMode(0o755)
	filePerms     = os.FileMode(0o640)
	// When bumping the version number, please add a comment indicating the
	// reason for the bump. Note that the version should only be bumped if the
	// new version introduces backwards-incompatible changes. Note that protobuf
	// changes are backwards-compatible, this version number is only necessary
	// for the non-proto format (e.g. if compression is introduced).
	// Version 1: Initial snapshot version with checksum and version number.
	snapshotVersion = 1
	// minReadVersion is bumped when deprecating older versions. For example,
	// a reader of the new version can choose to still support reading older
	// versions, but will bump this constant to the minimum version it claims
	// to support.
	minReadVersion = snapshotVersion
)

// segmentName returns a 20-byte textual representation of a snapshot file name
// at a given txn used for lexical ordering.
func snapshotFileName(tx uint64) string {
	return fmt.Sprintf("%020d.fdbs", tx)
}

// snapshot takes a snapshot of the state of the database at transaction tx.
func (db *DB) snapshot(ctx context.Context, tx uint64) error {
	if !db.columnStore.enableWAL {
		return nil
	}

	snapshotsDir := db.snapshotsDir()
	if err := os.MkdirAll(snapshotsDir, dirPerms); err != nil {
		return err
	}

	fileName := filepath.Join(snapshotsDir, snapshotFileName(tx))
	f, err := os.OpenFile(fileName, os.O_CREATE|os.O_RDWR|os.O_TRUNC, filePerms)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := func() error {
		if err := writeSnapshot(ctx, tx, db, f); err != nil {
			return err
		}
		return f.Sync()
	}(); err != nil {
		err = fmt.Errorf("failed to write snapshot for tx %d: %w", tx, err)
		if removeErr := os.Remove(fileName); removeErr != nil {
			err = fmt.Errorf("%w: failed to remove snapshot file: %v", err, removeErr)
		}
		return err
	}
	// TODO(asubiotto): If snapshot file sizes become too large, investigate
	// adding compression.
	return nil
}

// loadLatestSnapshot loads the latest snapshot (i.e. the snapshot with the
// highest txn) from the snapshots dir into the database.
func (db *DB) loadLatestSnapshot(ctx context.Context) (uint64, error) {
	return db.loadLatestSnapshotFromDir(ctx, db.snapshotsDir())
}

func (db *DB) loadLatestSnapshotFromDir(ctx context.Context, dir string) (uint64, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}

	if len(files) == 0 {
		return 0, nil
	}

	var lastErr error
	// Iterate in reverse order so that the newest snapshot is first.
	for i := len(files) - 1; i >= 0; i-- {
		entry := files[i]
		name := entry.Name()
		if entry.IsDir() || len(name) < 20 {
			continue
		}
		parsedTx, err := strconv.ParseUint(name[:20], 10, 64)
		if err != nil {
			continue
		}

		if err := func() error {
			f, err := os.Open(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			defer f.Close()
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if err := loadSnapshot(ctx, db, f, info.Size()); err != nil {
				return err
			}
			db.tx.Store(parsedTx)
			db.highWatermark.Store(parsedTx)
			return nil
		}(); err != nil {
			err = fmt.Errorf("unable to read snapshot file %s: %w", name, err)
			level.Info(db.logger).Log(
				"msg", "error reading snapshot",
				"error", err,
			)
			lastErr = err
			continue
		}
		// Success.
		return parsedTx, nil
	}
	errString := "no valid snapshots found"
	if lastErr != nil {
		return 0, fmt.Errorf("%s: lastErr: %w", errString, lastErr)
	}
	return 0, fmt.Errorf("%s", errString)
}

type offsetWriter struct {
	w               io.Writer
	runningChecksum hash.Hash32
	offset          int
}

func newChecksumWriter() hash.Hash32 {
	return crc32.New(crc32.MakeTable(crc32.Castagnoli))
}

func newOffsetWriter(w io.Writer) *offsetWriter {
	return &offsetWriter{
		w:               w,
		runningChecksum: newChecksumWriter(),
	}
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	if n, err := w.runningChecksum.Write(p); err != nil {
		return n, fmt.Errorf("error writing checksum: %w", err)
	}
	n, err := w.w.Write(p)
	w.offset += n
	return n, err
}

func (w *offsetWriter) checksum() uint32 {
	return w.runningChecksum.Sum32()
}

func writeSnapshot(ctx context.Context, tx uint64, db *DB, w io.Writer) error {
	offW := newOffsetWriter(w)
	w = offW
	var tables []*Table
	db.mtx.RLock()
	for _, t := range db.tables {
		tables = append(tables, t)
	}
	db.mtx.RUnlock()

	if _, err := w.Write([]byte(snapshotMagic)); err != nil {
		return err
	}

	metadata := &snapshotpb.FooterData{}
	for _, t := range tables {
		tableMeta := &snapshotpb.Table{
			Name: t.name,
			Config: &snapshotpb.Table_TableConfig{
				RowGroupSize:     int64(t.config.rowGroupSize),
				BlockReaderLimit: int64(t.config.blockReaderLimit),
				DisableWal:       t.config.disableWAL,
			},
		}
		switch v := t.config.schema.Definition().(type) {
		case *schemapb.Schema:
			tableMeta.Config.Schema = &snapshotpb.Table_TableConfig_DeprecatedSchema{
				DeprecatedSchema: v,
			}
		case *schemav2pb.Schema:
			tableMeta.Config.Schema = &snapshotpb.Table_TableConfig_SchemaV2{
				SchemaV2: v,
			}
		default:
			return fmt.Errorf("unknown schema type: %t", v)
		}

		var ascendErr error
		t.ActiveBlock().Index().Ascend(func(i btree.Item) bool {
			granuleMeta := &snapshotpb.Granule{}
			i.(*Granule).PartsForTx(tx, func(p *parts.Part) bool {
				partMeta := &snapshotpb.Part{
					StartOffset:     int64(offW.offset),
					Tx:              p.TX(),
					CompactionLevel: uint64(p.CompactionLevel()),
				}
				if err := func() error {
					if err := ctx.Err(); err != nil {
						return err
					}
					schema := t.config.schema

					if record := p.Record(); record != nil {
						partMeta.Encoding = snapshotpb.Part_ENCODING_ARROW
						recordWriter := ipc.NewWriter(
							w,
							ipc.WithSchema(record.Schema()),
						)
						defer recordWriter.Close()
						return recordWriter.Write(record)
					}
					partMeta.Encoding = snapshotpb.Part_ENCODING_PARQUET

					buf, err := p.AsSerializedBuffer(schema)
					if err != nil {
						return err
					}
					rows := buf.Reader()
					defer rows.Close()

					parquetWriter, err := schema.GetWriter(w, buf.DynamicColumns())
					if err != nil {
						return err
					}
					defer schema.PutWriter(parquetWriter)
					defer parquetWriter.Close()

					if _, err := parquet.CopyRows(parquetWriter, rows); err != nil {
						return err
					}
					return nil
				}(); err != nil {
					ascendErr = err
					return false
				}
				partMeta.EndOffset = int64(offW.offset)
				granuleMeta.PartMetadata = append(granuleMeta.PartMetadata, partMeta)
				return true
			})
			if len(granuleMeta.PartMetadata) > 0 {
				tableMeta.GranuleMetadata = append(tableMeta.GranuleMetadata, granuleMeta)
			}
			return true
		})
		if ascendErr != nil {
			return ascendErr
		}
		metadata.TableMetadata = append(metadata.TableMetadata, tableMeta)
	}
	footer, err := metadata.MarshalVT()
	if err != nil {
		return err
	}
	// Write footer + size.
	footer = binary.LittleEndian.AppendUint32(footer, uint32(len(footer)))
	if _, err := w.Write(footer); err != nil {
		return err
	}
	if _, err := w.Write(binary.LittleEndian.AppendUint32(nil, snapshotVersion)); err != nil {
		return err
	}
	if _, err := w.Write(binary.LittleEndian.AppendUint32(nil, offW.checksum())); err != nil {
		return err
	}
	if _, err := w.Write([]byte(snapshotMagic)); err != nil {
		return err
	}
	return nil
}

func readFooter(r io.ReaderAt, size int64) (*snapshotpb.FooterData, error) {
	buffer := make([]byte, 16)
	if _, err := r.ReadAt(buffer[:4], 0); err != nil {
		return nil, err
	}
	if string(buffer[:4]) != snapshotMagic {
		return nil, fmt.Errorf("invalid snapshot magic: %q", buffer[:4])
	}
	if _, err := r.ReadAt(buffer, size-int64(len(buffer))); err != nil {
		return nil, err
	}
	if string(buffer[12:]) != snapshotMagic {
		return nil, fmt.Errorf("invalid snapshot magic: %q", buffer[4:])
	}

	// The checksum does not include the last 8 bytes of the file, which is the
	// magic and the checksum. Create a section reader of all but the last 8
	// bytes to compute the checksum and validate it against the read checksum.
	checksum := binary.LittleEndian.Uint32(buffer[8:12])
	checksumWriter := newChecksumWriter()
	if _, err := io.Copy(checksumWriter, io.NewSectionReader(r, 0, size-8)); err != nil {
		return nil, fmt.Errorf("failed to compute checksum: %w", err)
	}
	if checksum != checksumWriter.Sum32() {
		return nil, fmt.Errorf(
			"snapshot file corrupt: invalid checksum: expected %x, got %x", checksum, checksumWriter.Sum32(),
		)
	}

	version := binary.LittleEndian.Uint32(buffer[4:8])
	if version > snapshotVersion {
		return nil, fmt.Errorf(
			"cannot read snapshot with version %d: max version supported: %d", version, snapshotVersion,
		)
	}
	if version < minReadVersion {
		return nil, fmt.Errorf(
			"cannot read snapshot with version %d: min version supported: %d", version, minReadVersion,
		)
	}

	footerSize := binary.LittleEndian.Uint32(buffer[:4])
	footerBytes := make([]byte, footerSize)
	if _, err := r.ReadAt(footerBytes, size-(int64(len(buffer))+int64(footerSize))); err != nil {
		return nil, err
	}
	footer := &snapshotpb.FooterData{}
	if err := footer.UnmarshalVT(footerBytes); err != nil {
		return nil, fmt.Errorf("could not unmarshal footer: %v", err)
	}
	return footer, nil
}

func loadSnapshot(ctx context.Context, db *DB, r io.ReaderAt, size int64) error {
	footer, err := readFooter(r, size)
	if err != nil {
		return err
	}

	db.compactorPool.pause()
	defer db.compactorPool.resume()

	for i, tableMeta := range footer.TableMetadata {
		if err := func() error {
			var schemaMsg proto.Message
			switch v := tableMeta.Config.Schema.(type) {
			case *snapshotpb.Table_TableConfig_DeprecatedSchema:
				schemaMsg = v.DeprecatedSchema
			case *snapshotpb.Table_TableConfig_SchemaV2:
				schemaMsg = v.SchemaV2
			default:
				return fmt.Errorf("unhandled schema type: %T", v)
			}
			schema, err := dynparquet.SchemaFromDefinition(schemaMsg)
			if err != nil {
				return err
			}

			options := []TableOption{
				WithRowGroupSize(int(tableMeta.Config.RowGroupSize)),
				WithBlockReaderLimit(int(tableMeta.Config.BlockReaderLimit)),
			}
			if tableMeta.Config.DisableWal {
				options = append(options, WithoutWAL())
			}
			tableConfig := NewTableConfig(
				schema,
				options...,
			)
			table, err := db.Table(tableMeta.Name, tableConfig)
			if err != nil {
				return err
			}

			table.mtx.Lock()
			block := table.active
			newIdx := block.Index().Clone()
			table.mtx.Unlock()

			for _, granuleMeta := range tableMeta.GranuleMetadata {
				resultParts := make([]*parts.Part, 0, len(granuleMeta.PartMetadata))
				for _, partMeta := range granuleMeta.PartMetadata {
					if err := ctx.Err(); err != nil {
						return err
					}
					startOffset := partMeta.StartOffset
					endOffset := partMeta.EndOffset
					partBytes := make([]byte, endOffset-startOffset)
					if _, err := r.ReadAt(partBytes, startOffset); err != nil {
						return err
					}
					partOptions := parts.WithCompactionLevel(parts.CompactionLevel(partMeta.CompactionLevel))
					switch partMeta.Encoding {
					case snapshotpb.Part_ENCODING_PARQUET:
						serBuf, err := dynparquet.ReaderFromBytes(partBytes)
						if err != nil {
							return err
						}
						resultParts = append(resultParts, parts.NewPart(partMeta.Tx, serBuf, partOptions))
					case snapshotpb.Part_ENCODING_ARROW:
						arrowReader, err := ipc.NewReader(bytes.NewReader(partBytes))
						if err != nil {
							return err
						}

						record, err := arrowReader.Read()
						if err != nil {
							return err
						}

						resultParts = append(resultParts, parts.NewArrowPart(partMeta.Tx, record, schema, partOptions))
					default:
						return fmt.Errorf("unknown part encoding: %s", partMeta.Encoding)
					}
				}

				granule, err := NewGranule(tableConfig, resultParts...)
				if err != nil {
					return err
				}
				newIdx.ReplaceOrInsert(granule)
			}

			// This shouldn't be necessary since compactions were paused and no
			// inserts should be happening, but err on the side of caution.
			for !block.index.CompareAndSwap(block.Index(), newIdx) {
			}

			return nil
		}(); err != nil {
			db.mtx.Lock()
			for _, cleanupTable := range footer.TableMetadata[:i] {
				delete(db.tables, cleanupTable.Name)
			}
			db.mtx.Unlock()
			return err
		}
	}

	return nil
}
