package response

import (
	"bytes"
	"encoding/csv"
	"strconv"
	"strings"
	"testing"
)

// FuzzStructsCSV checks that what StructsCSV writes reads back as the
// values it was given.
func FuzzStructsCSV(f *testing.F) {
	f.Add("Anna", int64(30), "Rome")
	f.Add("=SUM(A1)", int64(-5), "a,b\n\"c\"")
	f.Add("", int64(0), "")

	f.Fuzz(func(t *testing.T, name string, age int64, city string) {
		type row struct {
			Name string
			Age  int64
			City string
		}
		resp, err := StructsCSV([]row{{Name: name, Age: age, City: city}})
		if err != nil {
			t.Fatal(err)
		}

		records, err := csv.NewReader(bytes.NewReader(resp.Body)).ReadAll()
		if err != nil {
			t.Fatalf("unreadable CSV: %v: %q", err, resp.Body)
		}
		// encoding/csv reads a \r\n inside a field back as \n.
		want := []string{strings.ReplaceAll(name, "\r\n", "\n"), strconv.FormatInt(age, 10), strings.ReplaceAll(city, "\r\n", "\n")}
		if len(records) != 2 || strings.Join(records[1], "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("wrote %q, read back %q", want, records)
		}
	})
}
