package logictest

import (
	"context"
	"encoding/json"
	"io/fs"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow/go/v8/arrow/memory"
	"github.com/cockroachdb/datadriven"
	"github.com/stretchr/testify/require"

	"github.com/polarsignals/frostdb"
	"github.com/polarsignals/frostdb/dynparquet"
	schemapb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/schema/v1alpha1"
	"github.com/polarsignals/frostdb/query"
)

const (
	testdataDirectory = "testdata"
	schemasDirectory  = "schemas"
)

type frostDB struct {
	*frostdb.DB
}

func (db frostDB) CreateTable(name string, schema *dynparquet.Schema) (Table, error) {
	return db.DB.Table(name, frostdb.NewTableConfig(schema))
}

func (db frostDB) ScanTable(name string) query.Builder {
	queryEngine := query.NewEngine(
		memory.NewGoAllocator(),
		db.DB.TableProvider(),
	)
	return queryEngine.ScanTable(name)
}

// TestLogic runs all the datadriven tests in the testdata directory. Refer to
// the RunCmd method of the Runner struct for more information on the expected
// syntax of these tests. If this test fails but the results look the same, it
// might be because the test returns tab-separated expected results and your
// IDE inserts tabs instead of spaces. Just run this test with the -rewrite flag
// to rewrite expected results.
// TODO(asubiotto): Add metamorphic testing to logic tests. The idea is to
// randomly generate variables that should have no effect on output (e.g. row
// group split points, granule split size).
func TestLogic(t *testing.T) {
	ctx := context.Background()

	// Collect all the provided schemas
	schemas := map[string]*dynparquet.Schema{}
	filepath.Walk(schemasDirectory, func(path string, info fs.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}

		b, err := ioutil.ReadFile(path)
		require.NoError(t, err)

		var def schemapb.Schema
		require.NoError(t, json.Unmarshal(b, &def))

		schema, err := dynparquet.SchemaFromDefinition(&def)
		require.NoError(t, err)

		schemas[strings.TrimRight(filepath.Base(path), ".json")] = schema
		return nil
	})

	t.Parallel()
	datadriven.Walk(t, testdataDirectory, func(t *testing.T, path string) {
		columnStore, err := frostdb.New()
		require.NoError(t, err)
		defer columnStore.Close()
		db, err := columnStore.DB(ctx, "test")
		require.NoError(t, err)
		r := NewRunner(frostDB{DB: db}, schemas)
		datadriven.RunTest(t, path, func(t *testing.T, c *datadriven.TestData) string {
			return r.RunCmd(ctx, c)
		})
	})
}
