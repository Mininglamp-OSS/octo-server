package cardmsg

import "testing"

func TestCardTemplateContextReadsOnlyTypedMetadata(t *testing.T) {
	raw := []byte(`{
		"type":17,"profile":"octo/v2","card_version":"1.5",
		"card":{"type":"AdaptiveCard","version":"1.5","body":[],"metadata":{"octo":{
			"protocol":"octo-card@1.0","template":{"id":"docs.access-request","version":"0.2.0"}
		}}}
	}`)
	ctx, ok := CardTemplateContext(raw)
	if !ok || ctx.ID != "docs.access-request" || ctx.Version != "0.2.0" || ctx.Protocol != "octo-card@1.0" {
		t.Fatalf("CardTemplateContext() = (%+v, %v)", ctx, ok)
	}
	for _, malformed := range [][]byte{
		[]byte(`{"type":17,"card":{"metadata":{"octo":{"template":{"id":"docs.access-request"}}}}}`),
		[]byte(`{"type":17,"card":{"metadata":{"octo":{"protocol":"other","template":{"id":"docs.access-request","version":"0.2.0"}}}}}`),
		[]byte(`not-json`),
	} {
		if got, ok := CardTemplateContext(malformed); ok {
			t.Errorf("CardTemplateContext(%s) = %+v, want absent", malformed, got)
		}
	}
}
