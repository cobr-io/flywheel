// Package config merges flywheel.yaml with flywheel.yaml.local. Map keys
// deep-merge recursively; list-valued fields in .local replace the
// committed list **wholesale** (per design § flywheel.yaml: "arrays are
// replaced wholesale" — no concat, no merge-by-index, no merge-by-key).
package config

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

// MergeYAML returns the merged YAML bytes. `committed` and `local` are the
// raw bytes of flywheel.yaml and flywheel.yaml.local respectively. Either
// may be empty; an empty local is a no-op.
//
// Merge semantics:
//   - Scalars in `local` override `committed`.
//   - Maps merge recursively: every key present in `local` is merged into
//     `committed`; keys only in `committed` survive.
//   - **Lists in `local` replace the list at the same path** in `committed`
//     entirely.
//   - A null value in `local` deletes the field from the merged result.
func MergeYAML(committed, local []byte) ([]byte, error) {
	if len(local) == 0 {
		return committed, nil
	}
	var c, l any
	if len(committed) > 0 {
		if err := yaml.Unmarshal(committed, &c); err != nil {
			return nil, fmt.Errorf("parse committed: %w", err)
		}
	}
	if err := yaml.Unmarshal(local, &l); err != nil {
		return nil, fmt.Errorf("parse local: %w", err)
	}
	merged := merge(c, l)
	out, err := yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal merged: %w", err)
	}
	return out, nil
}

// merge applies the documented semantics. Exported for tests in this
// package (so they can exercise nested map/list edge cases without
// round-tripping through YAML).
func merge(committed, local any) any {
	// A null at this position deletes.
	if local == nil {
		return nil
	}
	// Different shapes: local wins outright.
	cMap, cIsMap := committed.(map[string]any)
	lMap, lIsMap := local.(map[string]any)
	if cIsMap && lIsMap {
		out := make(map[string]any, len(cMap)+len(lMap))
		for k, v := range cMap {
			out[k] = v
		}
		for k, v := range lMap {
			if existing, ok := out[k]; ok {
				out[k] = merge(existing, v)
			} else {
				out[k] = v
			}
			// Delete-on-null happens inside merge; if the recursive
			// call returned nil, drop the key.
			if out[k] == nil {
				delete(out, k)
			}
		}
		return out
	}
	// Lists: wholesale replace.
	if _, ok := local.([]any); ok {
		return local
	}
	if _, ok := committed.([]any); ok {
		return local
	}
	// Scalars: local wins.
	return local
}
