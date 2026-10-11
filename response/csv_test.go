package response

import (
	"database/sql/driver"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The names of the two rows the CSV tests write.
const (
	firstRowName  = "Anna"
	secondRowName = "Bob"
)

// csvLines splits a CSV body into its lines. A row whose only cell is empty
// is an empty line: encoding/csv writes it that way.
func csvLines(body string) []string {
	return strings.Split(strings.TrimSuffix(body, "\n"), "\n")
}

func TestCSV_HappyPath(t *testing.T) {
	t.Parallel()

	resp, err := CSV([][]string{
		{"name", "age"},
		{firstRowName, "30"},
		{secondRowName, "25"},
	})
	require.NoError(t, err)

	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/csv", resp.Header.Get(headerContentType))
	assert.Equal(t, `attachment; filename="data.csv"`, resp.Header.Get("Content-Disposition"))
	assert.Equal(t, []string{"name,age", "Anna,30", "Bob,25"}, csvLines(string(resp.Body)))
}

func TestCSV_EmptyData(t *testing.T) {
	t.Parallel()

	resp, err := CSV([][]string{})
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Empty(t, string(resp.Body))
}

func TestStructsCSV_ExportedFieldsOnly(t *testing.T) {
	t.Parallel()

	type row struct {
		ID       int
		Name     string
		hidden   string //nolint:unused // field intentionally unexported to verify it is skipped
		Optional *string
	}
	rows := []row{{ID: 1, Name: firstRowName, Optional: new("yes")}, {ID: 2, Name: secondRowName}}

	resp, err := StructsCSV(rows)
	require.NoError(t, err)

	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/csv", resp.Header.Get(headerContentType))
	assert.Equal(t, []string{"ID,Name,Optional", "1,Anna,yes", "2,Bob,"}, csvLines(string(resp.Body)))
}

func TestStructsCSV_EmptySliceKeepsTheHeader(t *testing.T) {
	t.Parallel()

	type row struct{ Name, City string }
	resp, err := StructsCSV([]row{})
	require.NoError(t, err)
	assert.Equal(t, []string{"Name,City"}, csvLines(string(resp.Body)))
}

func TestStructsCSV_SliceOfPointers(t *testing.T) {
	t.Parallel()

	type row struct{ Name string }
	resp, err := StructsCSV([]*row{{Name: firstRowName}, nil, {Name: secondRowName}})
	require.NoError(t, err)
	assert.Equal(t, []string{"Name", firstRowName, "", secondRowName}, csvLines(string(resp.Body)),
		"a nil element is a row of empty cells")
}

func TestStructsCSV_ValueFormats(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 10, 10, 12, 30, 0, 0, time.UTC)
	id := uuid.MustParse("0314e8c2-2031-7012-c18d-54c4aaad95aa")
	type row struct {
		Small   float32
		At      time.Time
		ID      uuid.UUID
		Stamp   pgtype.Timestamptz
		Missing pgtype.Text
	}
	resp, err := StructsCSV([]row{{
		Small: 0.1,
		At:    at,
		ID:    id,
		Stamp: pgtype.Timestamptz{Time: at, Valid: true},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"Small,At,ID,Stamp,Missing",
		"0.1,2026-10-10T12:30:00Z,0314e8c2-2031-7012-c18d-54c4aaad95aa,2026-10-10T12:30:00Z,",
	}, csvLines(string(resp.Body)))
}

func TestStructsCSV_RejectsNonSlice(t *testing.T) {
	t.Parallel()

	_, err := StructsCSV("not a slice")
	require.ErrorIs(t, err, errCSVNotASlice)
}

func TestStructsCSV_RejectsSliceOfNonStruct(t *testing.T) {
	t.Parallel()

	_, err := StructsCSV([]int{1, 2, 3})
	require.ErrorIs(t, err, errCSVNotStructs)
}

func TestValueToString_PointerVariants(t *testing.T) {
	t.Parallel()

	type row struct{ Field *string }
	resp, err := StructsCSV([]row{{Field: new("filled")}, {Field: nil}})
	require.NoError(t, err)
	assert.Equal(t, []string{"Field", "filled", ""}, csvLines(string(resp.Body)))
}

// selfValuer breaks the driver.Valuer contract by returning itself.
type selfValuer struct{}

func (s selfValuer) Value() (driver.Value, error) { return s, nil }

func TestStructsCSV_AValuerCannotLoopForever(t *testing.T) {
	t.Parallel()

	type row struct {
		Bad  selfValuer
		Blob pgtype.Text
	}
	resp, err := StructsCSV([]row{{Blob: pgtype.Text{String: "hi", Valid: true}}})
	require.NoError(t, err)
	assert.Equal(t, []string{"Bad,Blob", "{},hi"}, csvLines(string(resp.Body)))
}
