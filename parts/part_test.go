package parts

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/polarsignals/frostdb/dynparquet"
	schemapb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/schema/v1alpha1"
)

func TestFindMaximumNonOverlappingSet(t *testing.T) {
	testSchema, err := dynparquet.SchemaFromDefinition(&schemapb.Schema{
		Name: "test_schema",
		Columns: []*schemapb.Column{{
			Name: "ints",
			StorageLayout: &schemapb.StorageLayout{
				Type:     schemapb.StorageLayout_TYPE_INT64,
				Encoding: schemapb.StorageLayout_ENCODING_PLAIN_UNSPECIFIED,
			},
		}},
		SortingColumns: []*schemapb.SortingColumn{{Name: "ints", Direction: schemapb.SortingColumn_DIRECTION_ASCENDING}},
	})
	require.NoError(t, err)

	type rng struct {
		start int64
		end   int64
	}
	type dataModel struct {
		Ints int64
	}
	for _, tc := range []struct {
		name                   string
		ranges                 []rng
		expectedNonOverlapping []rng
		expectedOverlapping    []rng
	}{
		{
			name:                   "SinglePart",
			ranges:                 []rng{{1, 2}},
			expectedNonOverlapping: []rng{{1, 2}},
		},
		{
			name:                   "RemoveFirst",
			ranges:                 []rng{{1, 4}, {1, 2}},
			expectedNonOverlapping: []rng{{1, 2}},
			expectedOverlapping:    []rng{{1, 4}},
		},
		{
			name:                   "TwoNonOverlapping",
			ranges:                 []rng{{1, 2}, {3, 4}},
			expectedNonOverlapping: []rng{{1, 2}, {3, 4}},
		},
		{
			name:                   "OneOverlap",
			ranges:                 []rng{{1, 2}, {4, 7}, {3, 8}},
			expectedNonOverlapping: []rng{{1, 2}, {4, 7}},
			expectedOverlapping:    []rng{{3, 8}},
		},
		{
			name:                   "ChooseMinimumNumber",
			ranges:                 []rng{{1, 2}, {4, 10}, {4, 5}, {6, 7}},
			expectedNonOverlapping: []rng{{1, 2}, {4, 5}, {6, 7}},
			expectedOverlapping:    []rng{{4, 10}},
		},
		{
			// ReuseCursor makes sure that when dropping a range, its boundaries
			// are not reused. This is a regression test (which is why it's so
			// specific).
			name:                   "ReuseCursor",
			ranges:                 []rng{{1, 3}, {2, 4}, {4, 5}, {6, 7}},
			expectedNonOverlapping: []rng{{1, 3}, {4, 5}, {6, 7}},
			expectedOverlapping:    []rng{{2, 4}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := make([]*Part, len(tc.ranges))
			for i := range parts {
				start := dataModel{Ints: tc.ranges[i].start}
				end := dataModel{Ints: tc.ranges[i].end}
				buf, err := dynparquet.ValuesToBuffer(testSchema, start, end)
				require.NoError(t, err)
				var b bytes.Buffer
				require.NoError(t, testSchema.SerializeBuffer(&b, buf))
				serBuf, err := dynparquet.ReaderFromBytes(b.Bytes())
				require.NoError(t, err)
				parts[i] = NewPart(0, serBuf)
			}
			nonOverlapping, overlapping, err := FindMaximumNonOverlappingSet(testSchema, parts)
			require.NoError(t, err)

			verify := func(t *testing.T, expected []rng, actual []*Part) {
				t.Helper()
				require.Len(t, actual, len(expected))
				for i := range actual {
					start, err := actual[i].Least()
					require.NoError(t, err)
					end, err := actual[i].most()
					require.NoError(t, err)
					require.Equal(t, expected[i].start, start.Row[0].Int64())
					require.Equal(t, expected[i].end, end.Row[0].Int64())
				}
			}
			verify(t, tc.expectedNonOverlapping, nonOverlapping)
			verify(t, tc.expectedOverlapping, overlapping)
		})
	}
}
