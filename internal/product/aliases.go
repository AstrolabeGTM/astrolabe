package product

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Config accepts sales vocabulary as well as the developer terms: alias
// keys and values are rewritten to the canonical ones before the strict
// decode. Giving both forms of one setting is an error.

type alias struct {
	key    string            // alias key
	to     string            // canonical key
	values map[string]string // alias value -> canonical value (scalars)
}

var (
	autoApproveValues = map[string]string{"manual": "none", "first_step": "follow_ups", "auto": "all"}
	kindValues        = map[string]string{"outbound": "cold", "lifecycle": "users"}

	// Aliases per mapping path ("" is the document root).
	productAliases = map[string][]alias{
		"": {
			{key: "icp", to: "target"},
			{key: "autonomy", to: "auto_approve"},
		},
		"auto_approve": {
			{key: "outbound", to: "cold", values: autoApproveValues},
			{key: "lifecycle", to: "users", values: autoApproveValues},
			{key: "cold", to: "cold", values: autoApproveValues},
			{key: "users", to: "users", values: autoApproveValues},
			{key: "content", to: ""}, // accepted and ignored (content is always manual)
		},
		"sender": {
			{key: "outbound", to: "cold"},
			{key: "lifecycle", to: "users"},
		},
		"limits": {
			{key: "outbound_per_day", to: "cold_per_day"},
		},
	}
	sequenceAliases = map[string][]alias{
		"": {{key: "kind", to: "kind", values: kindValues}},
	}
)

// normalize rewrites alias keys/values in raw YAML and returns canonical YAML.
func normalize(raw []byte, aliases map[string][]alias) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return raw, nil
	}
	if err := walk(doc.Content[0], "", aliases); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func walk(n *yaml.Node, path string, aliases map[string][]alias) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	rules := aliases[path]
	var keep []*yaml.Node
	seen := map[string]string{} // canonical key -> key it came from
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		original := k.Value
		for _, a := range rules {
			if k.Value != a.key {
				continue
			}
			if a.to == "" {
				k = nil
				break
			}
			if a.values != nil && v.Kind == yaml.ScalarNode {
				if c, ok := a.values[v.Value]; ok {
					v.Value = c
				}
			}
			k.Value = a.to
			break
		}
		if k == nil {
			continue
		}
		if from, dup := seen[k.Value]; dup {
			return fmt.Errorf("%s and %s set the same thing; keep one", qualify(path, from), qualify(path, original))
		}
		seen[k.Value] = original
		if err := walk(v, join(path, k.Value), aliases); err != nil {
			return err
		}
		keep = append(keep, k, v)
	}
	n.Content = keep
	return nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func qualify(path, key string) string { return join(path, key) }
