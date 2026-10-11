package request

import (
	"errors"
	"strings"
)

// ErrTooManyCSVIDs is returned when the input carries more than max ids.
var ErrTooManyCSVIDs = errors.New("too many csv ids")

// InvalidCSVIDError carries the offending token, so a caller can put it in the
// details of its BadRequest, and the parse error behind it.
type InvalidCSVIDError struct {
	Value string
	Err   error
}

// Error implements the error interface.
func (e *InvalidCSVIDError) Error() string { return "invalid csv id: " + e.Value }

// Unwrap returns the parse error.
func (e *InvalidCSVIDError) Unwrap() error { return e.Err }

// ParseCSVIDs splits a comma-separated id list, trims each token, skips empties,
// enforces the cap of maxIDs ids, and parses each surviving token via parse.
// Empty tokens do not count towards the cap.
// It does NOT dedup (callers rely on `= ANY($1)`, where dups are harmless) and
// does NOT decide the empty-result policy: the caller inspects len(result).
func ParseCSVIDs[T any](raw string, maxIDs int, parse func(string) (T, error)) ([]T, error) {
	ids := []T{}
	for p := range strings.SplitSeq(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(ids) >= maxIDs {
			return nil, ErrTooManyCSVIDs
		}
		v, err := parse(p)
		if err != nil {
			return nil, &InvalidCSVIDError{Value: p, Err: err}
		}
		ids = append(ids, v)
	}
	return ids, nil
}
