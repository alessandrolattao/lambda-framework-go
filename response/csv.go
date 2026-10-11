package response

import (
	"bytes"
	"database/sql/driver"
	"encoding"
	"encoding/csv"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
)

// The errors of StructsCSV given something that is not a slice of structs.
var (
	errCSVNotASlice  = errors.New("data must be a slice")
	errCSVNotStructs = errors.New("slice elements must be structs")
)

// CSV answers 200 with records as a CSV document, downloaded as data.csv.
func CSV(records [][]string, opts ...Option) (eventhandler.Response[[]byte], error) {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	if err := writer.WriteAll(records); err != nil {
		return eventhandler.Response[[]byte]{}, fmt.Errorf("write CSV: %w", err)
	}
	resp := respond(http.StatusOK, buffer.Bytes(), opts)
	resp.Header.Set(headerContentType, "text/csv")
	resp.Header.Set("Content-Disposition", `attachment; filename="data.csv"`)
	return resp, nil
}

// StructsCSV answers 200 with rows, a slice of structs or of pointers to
// structs, as CSV: a header row with the exported field names, then a row
// per element (empty for a nil pointer). A field is written with its
// MarshalText, else its driver.Valuer, else its plain value.
func StructsCSV(rows any, opts ...Option) (eventhandler.Response[[]byte], error) {
	records, err := structRecords(rows)
	if err != nil {
		return eventhandler.Response[[]byte]{}, err
	}
	return CSV(records, opts...)
}

// Negotiate answers rows as CSV (StructsCSV) when accept, the request's
// Accept header, asks for text/csv, and as JSON otherwise. The response
// varies on Accept, so caches keep the two apart.
func Negotiate(accept string, rows any, opts ...Option) (eventhandler.Response[[]byte], error) {
	opts = append(opts, WithHeader("Vary", "Accept"))
	if strings.Contains(strings.ToLower(accept), "text/csv") {
		return StructsCSV(rows, opts...)
	}
	encoded, err := json.Marshal(rows, json.Deterministic(true))
	if err != nil {
		return eventhandler.Response[[]byte]{}, fmt.Errorf("encode %T: %w", rows, err)
	}
	resp := respond(http.StatusOK, encoded, opts)
	resp.Header.Set(headerContentType, contentTypeJSON)
	return resp, nil
}

// structRecords turns a slice of structs into CSV records, the header first.
func structRecords(rows any) ([][]string, error) {
	v := reflect.ValueOf(rows)
	if v.Kind() != reflect.Slice {
		return nil, fmt.Errorf("%w, got %T", errCSVNotASlice, rows)
	}

	elem := v.Type().Elem()
	if elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}
	if elem.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w, got %T", errCSVNotStructs, rows)
	}

	var fields []reflect.StructField
	for field := range elem.Fields() {
		if field.IsExported() {
			fields = append(fields, field)
		}
	}

	records := make([][]string, 0, v.Len()+1)
	header := make([]string, len(fields))
	for i, field := range fields {
		header[i] = field.Name
	}
	records = append(records, header)

	for _, row := range v.Seq2() {
		record := make([]string, len(fields))
		if row.Kind() == reflect.Pointer {
			if row.IsNil() {
				records = append(records, record)
				continue
			}
			row = row.Elem()
		}
		for j, field := range fields {
			record[j] = valueToString(row.FieldByIndex(field.Index))
		}
		records = append(records, record)
	}
	return records, nil
}

// valueToString converts a reflect.Value to its string representation for CSV.
func valueToString(v reflect.Value) string {
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
		return ""
	}
	if s, ok := marshaledString(v); ok {
		return s
	}

	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return valueToString(v.Elem())
	case reflect.String:
		return v.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, v.Type().Bits())
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	default:
		return fmt.Sprint(v.Interface())
	}
}

// marshaledString writes v through the methods a type uses to say how it is
// written: MarshalText first, then driver.Valuer.
func marshaledString(v reflect.Value) (string, bool) {
	if !v.CanInterface() {
		return "", false
	}
	value := v.Interface()
	if v.CanAddr() {
		value = v.Addr().Interface()
	}

	if m, ok := value.(encoding.TextMarshaler); ok {
		if text, err := m.MarshalText(); err == nil {
			return string(text), true
		}
	}
	if valuer, ok := value.(driver.Valuer); ok {
		inner, err := valuer.Value()
		if err != nil {
			return "", false
		}
		return driverValueString(inner), true
	}
	return "", false
}

// driverValueString writes one of the types a driver.Value holds. It does not
// recurse: a Valuer that broke the contract cannot loop it forever.
func driverValueString(v driver.Value) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(x)
	}
}
