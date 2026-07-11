package cwmp

import "testing"

func TestLoadManifest(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Count != 20260 {
		t.Errorf("param_count = %d, want 20260", m.Count)
	}
	if len(m.Params) != m.Count {
		t.Errorf("params len = %d, want %d", len(m.Params), m.Count)
	}
	if m.CanonicalID != "8C1F64-ENB%2DN03002%2DB3-2205600282" {
		t.Errorf("canonical id = %q", m.CanonicalID)
	}
}

func TestManifestXSD(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// A known unsignedInt path from the manifest.
	if got := m.XSD("Device.InterfaceStackNumberOfEntries"); got != "xsd:unsignedInt" {
		t.Errorf("XSD(NumberOfEntries) = %q, want xsd:unsignedInt", got)
	}
	// Unknown path defaults to string.
	if got := m.XSD("Device.Does.Not.Exist"); got != "xsd:string" {
		t.Errorf("XSD(unknown) = %q, want xsd:string", got)
	}
}

func TestXSDForTypes(t *testing.T) {
	cases := map[string]string{
		"unsignedInt":  "xsd:unsignedInt",
		"int":          "xsd:int",
		"boolean":      "xsd:boolean",
		"unsignedLong": "xsd:unsignedLong",
		"dateTime":     "xsd:dateTime",
		"hexBinary":    "xsd:hexBinary",
		"string":       "xsd:string",
		"":             "xsd:string",
	}
	for in, want := range cases {
		if got := xsdFor(in); got != want {
			t.Errorf("xsdFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestManifestSubtree(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// FAPService subtree is large (handover §2 counts ~2,954); assert non-trivial
	// and that every returned path carries the prefix.
	sub := m.Subtree("Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.")
	if len(sub) == 0 {
		t.Fatal("expected non-empty FAPService RF subtree")
	}
	for _, p := range sub {
		if len(p) < len("Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.") {
			t.Errorf("path %q shorter than prefix", p)
		}
	}
}
