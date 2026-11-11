package sst

import (
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/sst/sst/v3/sdk/golang/resource"
)

// errNotString is the cause of a secret whose value is not a string.
var errNotString = errors.New("value is not a string")

// Secret returns the value of the sst.Secret linked to the function as name,
// the name the secret has in sst.config.ts.
//
// A secret that is not linked, or that has no value, is an error wrapping
// resource.ErrNotFound of SST's SDK; a value that is not a string is an error
// too.
func Secret(name string) (string, error) {
	v, err := resource.Get(name, "value")
	if err != nil {
		return "", fmt.Errorf("sst: secret %q: %w", name, err)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("sst: secret %q: %w: %T", name, errNotString, v)
	}
	return s, nil
}

// Get decodes the resource linked to the function as name, the name the
// component has in sst.config.ts, into the value out points to, as
// json.Unmarshal of encoding/json/v2 does: out is a pointer, typically to a
// struct whose JSON tags name the properties the component links.
//
// A resource that is not linked is an error wrapping resource.ErrNotFound of
// SST's SDK.
func Get(name string, out any) error {
	v, err := resource.Get(name)
	if err != nil {
		return fmt.Errorf("sst: linked resource %q: %w", name, err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("sst: decode linked resource %q: %w", name, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("sst: decode linked resource %q: %w", name, err)
	}
	return nil
}
