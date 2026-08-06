package cardmsg

import (
	"encoding/json"
	"strings"
	"testing"
)

// Slice 1 RED (PR-C D3): the server-authored top-level catalog markers are a
// strict wire value object. Anything that does not match the canonical v1
// shape must fail parsing — a malformed marker is a tamper signal, never a
// legacy fallback.

func validProvenanceMap() map[string]interface{} {
	return map[string]interface{}{
		"version":        1,
		"principal_type": "internal_producer",
		"principal_id":   "docs-notify",
		"space_id":       "space-1",
	}
}

func TestParseCatalogProvenanceStrictShape(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]interface{})
		value   interface{}
		wantErr bool
	}{
		{name: "canonical internal producer", mutate: func(m map[string]interface{}) {}},
		{name: "canonical bot", mutate: func(m map[string]interface{}) {
			m["principal_type"] = "bot"
			m["principal_id"] = "bot-1"
		}},
		{name: "empty space is allowed pre target-space resolution", mutate: func(m map[string]interface{}) {
			m["space_id"] = ""
		}},
		{name: "json.Number version from UseNumber decode", mutate: func(m map[string]interface{}) {
			m["version"] = json.Number("1")
		}},
		{name: "float64 version from plain decode", mutate: func(m map[string]interface{}) {
			m["version"] = float64(1)
		}},
		{name: "not a map", value: "forged", wantErr: true},
		{name: "nil value", value: nil, wantErr: true},
		{name: "unknown extra key", mutate: func(m map[string]interface{}) {
			m["actor"] = "attacker"
		}, wantErr: true},
		{name: "missing version", mutate: func(m map[string]interface{}) {
			delete(m, "version")
		}, wantErr: true},
		{name: "unsupported version", mutate: func(m map[string]interface{}) {
			m["version"] = 2
		}, wantErr: true},
		{name: "missing principal_type", mutate: func(m map[string]interface{}) {
			delete(m, "principal_type")
		}, wantErr: true},
		{name: "unknown principal_type", mutate: func(m map[string]interface{}) {
			m["principal_type"] = "space"
		}, wantErr: true},
		{name: "system principal is not a wire principal", mutate: func(m map[string]interface{}) {
			m["principal_type"] = "system"
		}, wantErr: true},
		{name: "missing principal_id", mutate: func(m map[string]interface{}) {
			delete(m, "principal_id")
		}, wantErr: true},
		{name: "empty principal_id", mutate: func(m map[string]interface{}) {
			m["principal_id"] = ""
		}, wantErr: true},
		{name: "untrimmed principal_id", mutate: func(m map[string]interface{}) {
			m["principal_id"] = " docs-notify"
		}, wantErr: true},
		{name: "missing space_id key", mutate: func(m map[string]interface{}) {
			delete(m, "space_id")
		}, wantErr: true},
		{name: "untrimmed space_id", mutate: func(m map[string]interface{}) {
			m["space_id"] = "space-1 "
		}, wantErr: true},
		{name: "non-string principal_id", mutate: func(m map[string]interface{}) {
			m["principal_id"] = 42
		}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.value
			if tc.mutate != nil {
				m := validProvenanceMap()
				tc.mutate(m)
				value = m
			}
			parsed, err := ParseCatalogProvenance(value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseCatalogProvenance(%#v) accepted a non-canonical marker: %+v", value, parsed)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCatalogProvenance(%#v): %v", value, err)
			}
			if parsed.Version != CatalogProvenanceVersion {
				t.Fatalf("version = %d", parsed.Version)
			}
		})
	}
}

func TestCatalogProvenanceMarshalRoundTrip(t *testing.T) {
	original := CatalogProvenance{
		Version:       CatalogProvenanceVersion,
		PrincipalType: CatalogPrincipalWireBot,
		PrincipalID:   "bot-7",
		SpaceID:       "space-9",
	}
	parsed, err := ParseCatalogProvenance(original.MarshalMap())
	if err != nil {
		t.Fatalf("round trip parse: %v", err)
	}
	if parsed != original {
		t.Fatalf("round trip = %+v, want %+v", parsed, original)
	}
	// The map must survive a JSON encode/decode cycle (durable event storage).
	raw, err := json.Marshal(original.MarshalMap())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	parsed, err = ParseCatalogProvenance(decoded)
	if err != nil {
		t.Fatalf("post-JSON parse: %v", err)
	}
	if parsed != original {
		t.Fatalf("post-JSON round trip = %+v, want %+v", parsed, original)
	}
}

func registryFrame(t *testing.T, mutate func(map[string]interface{})) []byte {
	t.Helper()
	frame := map[string]interface{}{
		"type":         17,
		"card_version": CardVersion,
		"profile":      "octo/v1",
		"template_ref": map[string]interface{}{"id": "docs.access-request", "version": "0.3.0"},
		"catalog_provenance": map[string]interface{}{
			"version": 1, "principal_type": "internal_producer",
			"principal_id": "docs-notify", "space_id": "space-1",
		},
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": CardVersion,
			"body": []interface{}{map[string]interface{}{"type": "TextBlock", "text": "hi"}},
			"metadata": map[string]interface{}{"octo": map[string]interface{}{
				"protocol": "octo-card@1.0",
				"template": map[string]interface{}{"id": "docs.access-request", "version": "0.3.0"},
			}},
		},
	}
	if mutate != nil {
		mutate(frame)
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCatalogFrameMarkersMatrix(t *testing.T) {
	t.Run("both markers canonical", func(t *testing.T) {
		markers, err := CatalogFrameMarkers(registryFrame(t, nil))
		if err != nil {
			t.Fatalf("CatalogFrameMarkers: %v", err)
		}
		if !markers.HasRef || !markers.HasProvenance {
			t.Fatalf("markers presence = %+v", markers)
		}
		if markers.Ref.ID != "docs.access-request" || markers.Ref.Version != "0.3.0" {
			t.Fatalf("ref = %+v", markers.Ref)
		}
		if markers.Provenance.PrincipalID != "docs-notify" || markers.Provenance.SpaceID != "space-1" {
			t.Fatalf("provenance = %+v", markers.Provenance)
		}
	})
	t.Run("legacy frame without markers", func(t *testing.T) {
		markers, err := CatalogFrameMarkers(registryFrame(t, func(f map[string]interface{}) {
			delete(f, "template_ref")
			delete(f, "catalog_provenance")
		}))
		if err != nil {
			t.Fatalf("legacy frame must stay compatible: %v", err)
		}
		if markers.HasRef || markers.HasProvenance {
			t.Fatalf("legacy frame reported markers: %+v", markers)
		}
	})
	t.Run("pre-PR-C bot frame keeps ref-only compatibility", func(t *testing.T) {
		markers, err := CatalogFrameMarkers(registryFrame(t, func(f map[string]interface{}) {
			delete(f, "catalog_provenance")
		}))
		if err != nil {
			t.Fatalf("ref-only frame must stay compatible: %v", err)
		}
		if !markers.HasRef || markers.HasProvenance {
			t.Fatalf("ref-only markers = %+v", markers)
		}
	})
	t.Run("provenance without ref is never a server shape", func(t *testing.T) {
		if _, err := CatalogFrameMarkers(registryFrame(t, func(f map[string]interface{}) {
			delete(f, "template_ref")
		})); err == nil {
			t.Fatal("provenance without template_ref accepted")
		}
	})
	t.Run("ref mismatching metadata is rejected", func(t *testing.T) {
		if _, err := CatalogFrameMarkers(registryFrame(t, func(f map[string]interface{}) {
			f["template_ref"] = map[string]interface{}{"id": "docs.access-request", "version": "0.2.0"}
		})); err == nil {
			t.Fatal("template_ref/metadata version mismatch accepted")
		}
	})
	t.Run("ref without template metadata is rejected", func(t *testing.T) {
		if _, err := CatalogFrameMarkers(registryFrame(t, func(f map[string]interface{}) {
			card := f["card"].(map[string]interface{})
			delete(card, "metadata")
		})); err == nil {
			t.Fatal("template_ref without metadata.octo.template accepted")
		}
	})
	t.Run("malformed provenance is rejected", func(t *testing.T) {
		if _, err := CatalogFrameMarkers(registryFrame(t, func(f map[string]interface{}) {
			f["catalog_provenance"] = "forged"
		})); err == nil {
			t.Fatal("malformed catalog_provenance accepted")
		}
	})
	t.Run("malformed ref is rejected", func(t *testing.T) {
		if _, err := CatalogFrameMarkers(registryFrame(t, func(f map[string]interface{}) {
			f["template_ref"] = map[string]interface{}{"id": "docs.access-request"}
		})); err == nil {
			t.Fatal("template_ref missing version accepted")
		}
	})
	t.Run("non-card payload has no markers", func(t *testing.T) {
		markers, err := CatalogFrameMarkers([]byte(`{"type":1,"content":"hello"}`))
		if err != nil || markers.HasRef || markers.HasProvenance {
			t.Fatalf("non-card = %+v err=%v", markers, err)
		}
	})
}

// Validate must reject malformed markers as defense in depth on every path
// that funnels a full envelope through it (send + NormalizeContentEdit).
func TestValidateRejectsMalformedCatalogMarkers(t *testing.T) {
	decode := func(raw []byte) map[string]interface{} {
		var payload map[string]interface{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	if err := Validate(decode(registryFrame(t, nil))); err != nil {
		t.Fatalf("canonical markers must pass Validate: %v", err)
	}
	for name, mutate := range map[string]func(map[string]interface{}){
		"provenance not object": func(f map[string]interface{}) { f["catalog_provenance"] = 7 },
		"provenance extra key": func(f map[string]interface{}) {
			f["catalog_provenance"].(map[string]interface{})["grantor"] = "x"
		},
		"provenance without ref": func(f map[string]interface{}) { delete(f, "template_ref") },
		"ref not object":         func(f map[string]interface{}) { f["template_ref"] = []interface{}{} },
		"ref metadata mismatch": func(f map[string]interface{}) {
			f["template_ref"].(map[string]interface{})["version"] = "9.9.9"
		},
	} {
		t.Run(name, func(t *testing.T) {
			payload := decode(registryFrame(t, nil))
			mutate(payload)
			err := Validate(payload)
			if err == nil {
				t.Fatal("Validate accepted a malformed catalog marker")
			}
			if !strings.Contains(err.Error(), "catalog") {
				t.Fatalf("error should identify the catalog marker: %v", err)
			}
		})
	}
}
