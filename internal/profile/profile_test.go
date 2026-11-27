package profile

import (
	"strings"
	"testing"
)

func TestLoadValidatesFaultGroups(t *testing.T) {
	base := `{"name": "x", "registers": [], "faults": [%s]}`
	ok := `{"group": "Device fault", "registers": [103, 104], "function_code": 3, "kind": "bits", "severity": "fault", "names": {"1": "Fan failure"}}`
	p, err := Load([]byte(strings.Replace(base, "%s", ok, 1)))
	if err != nil {
		t.Fatalf("valid group: %v", err)
	}
	if g := p.Faults[0]; g.Names["1"] != "Fan failure" || len(g.Registers) != 2 {
		t.Errorf("parsed group = %+v", g)
	}

	bad := map[string]string{
		"no registers":   `{"group": "g", "registers": [], "function_code": 3, "kind": "bits", "severity": "fault"}`,
		"bad kind":       `{"group": "g", "registers": [1], "function_code": 3, "kind": "mask", "severity": "fault"}`,
		"bad fc":         `{"group": "g", "registers": [1], "function_code": 6, "kind": "bits", "severity": "fault"}`,
		"bad severity":   `{"group": "g", "registers": [1], "function_code": 3, "kind": "bits", "severity": "info"}`,
		"long code":      `{"group": "g", "registers": [1, 2, 3], "function_code": 3, "kind": "code", "severity": "fault"}`,
		"bad severities": `{"group": "g", "registers": [1], "function_code": 3, "kind": "bits", "severity": "fault", "severities": {"2": "major"}}`,
		"unnamed group":  `{"registers": [1], "function_code": 3, "kind": "bits", "severity": "fault"}`,
	}
	for name, g := range bad {
		if _, err := Load([]byte(strings.Replace(base, "%s", g, 1))); err == nil {
			t.Errorf("%s: Load accepted it", name)
		}
	}
}
