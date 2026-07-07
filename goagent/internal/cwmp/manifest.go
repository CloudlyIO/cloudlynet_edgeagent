package cwmp

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// nanolink_param_manifest.json is the full NanoLink parameter tree (20,260
// params) captured from the device. Embedded so the agent ships the catalogue
// with no external file dependency. The asset lives inside this package dir
// because go:embed patterns may not contain ".." path elements.
//
//go:embed assets/nanolink_param_manifest.json
var manifestBytes []byte

// ParamMeta is one entry of the embedded manifest. JSON keys are terse (p/v/t/w)
// to keep the 2.2 MB asset small.
type ParamMeta struct {
	Path     string `json:"p"`
	Value    string `json:"v"`
	Type     string `json:"t"` // string|unsignedInt|int|boolean|dateTime|unsignedLong|hexBinary
	Writable bool   `json:"w"` // heuristic candidate; runtime GetParameterNames overrides
}

type Manifest struct {
	CanonicalID string      `json:"cwmp_id_canonical"`
	Count       int         `json:"param_count"`
	Params      []ParamMeta `json:"params"`
	byPath      map[string]ParamMeta
}

// LoadManifest parses the embedded manifest and builds the path index.
func LoadManifest() (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("cwmp manifest load: %w", err)
	}
	m.byPath = make(map[string]ParamMeta, len(m.Params))
	for _, p := range m.Params {
		m.byPath[p.Path] = p
	}
	return &m, nil
}

// XSD returns the xsi:type for a parameter path, defaulting to xsd:string when
// the path is unknown (safest cast for a device write).
func (m *Manifest) XSD(path string) string {
	if p, ok := m.byPath[path]; ok {
		return xsdFor(p.Type)
	}
	return "xsd:string"
}

func xsdFor(t string) string {
	switch t {
	case "unsignedInt":
		return "xsd:unsignedInt"
	case "int":
		return "xsd:int"
	case "boolean":
		return "xsd:boolean"
	case "unsignedLong":
		return "xsd:unsignedLong"
	case "dateTime":
		return "xsd:dateTime"
	case "hexBinary":
		return "xsd:hexBinary"
	default:
		return "xsd:string"
	}
}

// Subtree returns every manifest path under prefix (inclusive of an exact match).
func (m *Manifest) Subtree(prefix string) []string {
	var out []string
	for _, p := range m.Params {
		if strings.HasPrefix(p.Path, prefix) {
			out = append(out, p.Path)
		}
	}
	return out
}
