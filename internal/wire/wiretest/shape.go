package wiretest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
)

// SameShape reports how got differs in shape from the golden JSON value: a missing or extra key, a
// different JSON kind (null where an array is wanted counts), or a different value under one of the
// fixed keys. Arrays are compared by their first element when both have one.
func SameShape(golden, got []byte, fixed ...string) error {
	var g, v any
	if err := json.Unmarshal(golden, &g); err != nil {
		return fmt.Errorf("golden: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(got))
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("body is not JSON: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("body has trailing data")
	}
	return sameShape("$", g, v, fixed)
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func sameShape(path string, g, v any, fixed []string) error {
	if kind(g) != kind(v) {
		return fmt.Errorf("%s: %s, want %s", path, kind(v), kind(g))
	}
	switch g := g.(type) {
	case map[string]any:
		v := v.(map[string]any)
		var keys []string
		for k := range g {
			keys = append(keys, k)
		}
		for k := range v {
			if _, ok := g[k]; !ok {
				return fmt.Errorf("%s: extra key %q", path, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			vv, ok := v[k]
			if !ok {
				return fmt.Errorf("%s: missing key %q", path, k)
			}
			p := path + "." + k
			if slices.Contains(fixed, k) && kind(g[k]) != "object" && kind(g[k]) != "array" && g[k] != vv {
				return fmt.Errorf("%s: %v, want %v", p, vv, g[k])
			}
			if err := sameShape(p, g[k], vv, fixed); err != nil {
				return err
			}
		}
	case []any:
		v := v.([]any)
		if len(g) > 0 && len(v) > 0 {
			return sameShape(path+"[0]", g[0], v[0], fixed)
		}
	}
	return nil
}
