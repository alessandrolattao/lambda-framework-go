package request

import (
	"errors"
	"strconv"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

func TestParseCSVIDs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		maxIDs  int
		want    []int64
		wantErr error
		wantBad string // the token InvalidCSVIDError reports
	}{
		{name: "ids", raw: "1,2,3", maxIDs: 10, want: []int64{1, 2, 3}},
		{name: "blanks and spaces are skipped", raw: " 1 , , 2 ,", maxIDs: 10, want: []int64{1, 2}},
		{name: "empty input", raw: "", maxIDs: 10, want: []int64{}},
		{name: "only blanks", raw: " , , ", maxIDs: 10, want: []int64{}},
		{name: "blanks do not count towards the cap", raw: "1,,,2", maxIDs: 2, want: []int64{1, 2}},
		{name: "a fourth id trips a cap of 3, even an invalid one", raw: "1,2,3,bad", maxIDs: 3, wantErr: ErrTooManyCSVIDs},
		{name: "an invalid token is reported verbatim", raw: "1,abc,3", maxIDs: 10, wantErr: strconv.ErrSyntax, wantBad: "abc"},
		{name: "an invalid token is reported trimmed", raw: " abc ", maxIDs: 10, wantErr: strconv.ErrSyntax, wantBad: "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ids, err := ParseCSVIDs(tc.raw, tc.maxIDs, parseInt64)
			if tc.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t, tc.want, ids, "never nil, even when empty")
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			assert.Nil(t, ids)
			if tc.wantBad != "" {
				invalid, ok := errors.AsType[*InvalidCSVIDError](err)
				require.True(t, ok)
				assert.Equal(t, tc.wantBad, invalid.Value)
			}
		})
	}
}

func TestParseCSVIDs_UUIDs(t *testing.T) {
	t.Parallel()

	a, b := uuid.New(), uuid.New()
	ids, err := ParseCSVIDs(a.String()+","+b.String(), 10, uuid.Parse)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{a, b}, ids)

	_, err = ParseCSVIDs("not-a-uuid", 10, uuid.Parse)
	invalid, ok := errors.AsType[*InvalidCSVIDError](err)
	require.True(t, ok)
	assert.Equal(t, "not-a-uuid", invalid.Value)
	assert.NotErrorIs(t, err, ErrTooManyCSVIDs, "a distinct error from the cap")
}
