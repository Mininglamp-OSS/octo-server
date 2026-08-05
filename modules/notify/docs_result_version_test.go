package notify

// PR-C review follow-up evidence: the click path and the mutate path resolve
// the result-edit version through one rule, and that rule refuses the two cases
// that used to fail deep inside the renderer.

import (
	"errors"
	"testing"

	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

func TestDocsResultVersionRules(t *testing.T) {
	for _, test := range []struct {
		name    string
		id      string
		version string
		want    string
		wantErr bool
	}{
		{
			name: "an unmarked legacy frame keeps rendering V3",
			want: docsaccessrequest.TemplateVersionV3,
		},
		{
			name: "a marked frame renders at its own stored version",
			id:   string(docsaccessrequest.TemplateID), version: docsaccessrequest.TemplateVersionV3,
			want: docsaccessrequest.TemplateVersionV3,
		},
		{
			// A dynamic pilot version is not in the static registry, and that is
			// fine: the stored version is authoritative and the catalog fails
			// loudly on its own if it cannot render.
			name: "a dynamic version is honoured verbatim",
			id:   string(docsaccessrequest.TemplateID), version: "0.4.0-pilot.20260805",
			want: "0.4.0-pilot.20260805",
		},
		{
			name: "0.2.0 has no result view and is refused with the reason",
			id:   string(docsaccessrequest.TemplateID), version: docsaccessrequest.TemplateVersion,
			wantErr: true,
		},
		{
			name: "another template is a routing mistake",
			id:   "ai.reasoning-process", version: "0.3.0",
			wantErr: true,
		},
		{
			name:    "a marked frame with no version cannot be resolved",
			id:      string(docsaccessrequest.TemplateID),
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := docsResultVersion(test.id, test.version)
			if test.wantErr {
				if !errors.Is(err, errDocsResultVersion) {
					t.Fatalf("error = %v, want errDocsResultVersion", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("docsResultVersion: %v", err)
			}
			if got != test.want {
				t.Fatalf("version = %q, want %q", got, test.want)
			}
		})
	}
}

// The two callers must not be able to drift apart again: both go through
// docsResultVersion, so the same stored identity yields the same answer.
func TestDocsResultVersionFromFrameMatchesTheStoredContext(t *testing.T) {
	unmarked := []byte(`{"type":17,"card":{"type":"AdaptiveCard"}}`)
	got, err := docsResultVersionFromFrame(unmarked)
	if err != nil || got != docsaccessrequest.TemplateVersionV3 {
		t.Fatalf("unmarked frame = %q err=%v, want the legacy V3 branch", got, err)
	}

	marked := []byte(`{"type":17,"template_ref":{"id":"docs.access-request","version":"0.3.0"},` +
		`"card":{"metadata":{"octo":{"protocol":"octo-card@1.0",` +
		`"template":{"id":"docs.access-request","version":"0.3.0"}}}}}`)
	got, err = docsResultVersionFromFrame(marked)
	if err != nil {
		t.Fatalf("marked frame: %v", err)
	}
	want, err := docsResultVersion("docs.access-request", "0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("frame path = %q, stored-context path = %q", got, want)
	}
}
