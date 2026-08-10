package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// nanolink_param_manifest.json is a copy of the in-repo, production-embedded
// manifest (goagent/internal/cwmp/assets/nanolink_param_manifest.json),
// synced via `make sync-manifest`. testsuite is a separate Go module and
// cannot import goagent/internal, so it embeds its own copy; that in-repo
// manifest is the single source of truth.
//
//go:embed assets/nanolink_param_manifest.json
var manifestBytes []byte

type manifestParam struct {
	Path     string `json:"p"`
	Value    string `json:"v"`
	Type     string `json:"t"` // string|unsignedInt|int|boolean|dateTime|unsignedLong|hexBinary
	Writable bool   `json:"w"`
}

// manifest is the parsed NanoLink parameter tree (20,260 params), seeding the
// mock device's store so a GetParameterValues for any managed path returns a
// realistic value + xsi:type.
type manifest struct {
	Params []manifestParam `json:"params"`
	byPath map[string]manifestParam
}

func loadManifest() (*manifest, error) {
	var m manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("testsuite manifest load: %w", err)
	}
	m.byPath = make(map[string]manifestParam, len(m.Params))
	for _, p := range m.Params {
		m.byPath[p.Path] = p
	}
	return &m, nil
}

// seedParams returns a path->value map covering every manifest entry.
func (m *manifest) seedParams() map[string]string {
	out := make(map[string]string, len(m.Params))
	for _, p := range m.Params {
		out[p.Path] = p.Value
	}
	return out
}

// xsdType returns the xsi:type for path, defaulting to xsd:string when the
// path is unknown to the manifest (safest cast for a device value).
func (m *manifest) xsdType(path string) string {
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
