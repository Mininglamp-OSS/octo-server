package cardtmpl_test

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

// Slice 1 RED (PR-C D3): the wire marker maps onto exactly one typed catalog
// principal. Unknown or non-sendable wire principals must not become catalog
// principals — the mapping is the only bridge between stored frames and
// authorization input.
func TestCatalogPrincipalFromProvenance(t *testing.T) {
	cases := []struct {
		name       string
		provenance cardmsg.CatalogProvenance
		want       cardtmpl.CatalogPrincipal
		wantErr    bool
	}{
		{
			name: "bot",
			provenance: cardmsg.CatalogProvenance{
				Version:       cardmsg.CatalogProvenanceVersion,
				PrincipalType: cardmsg.CatalogPrincipalWireBot,
				PrincipalID:   "bot-1", SpaceID: "space-1",
			},
			want: cardtmpl.CatalogPrincipal{
				Kind: cardtmpl.CatalogPrincipalBot, ID: "bot-1", SpaceID: "space-1",
			},
		},
		{
			name: "internal producer with empty space",
			provenance: cardmsg.CatalogProvenance{
				Version:       cardmsg.CatalogProvenanceVersion,
				PrincipalType: cardmsg.CatalogPrincipalWireInternalProducer,
				PrincipalID:   "docs-notify",
			},
			want: cardtmpl.CatalogPrincipal{
				Kind: cardtmpl.CatalogPrincipalInternalProducer, ID: "docs-notify",
			},
		},
		{
			name: "invalid marker never maps",
			provenance: cardmsg.CatalogProvenance{
				Version: 99, PrincipalType: cardmsg.CatalogPrincipalWireBot, PrincipalID: "bot-1",
			},
			wantErr: true,
		},
		{
			name: "system is not a stored-frame principal",
			provenance: cardmsg.CatalogProvenance{
				Version:       cardmsg.CatalogProvenanceVersion,
				PrincipalType: "system", PrincipalID: "readiness",
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cardtmpl.CatalogPrincipalFromProvenance(tc.provenance)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mapped invalid provenance to %+v", got)
				}
				if !errors.Is(err, cardmsg.ErrCatalogMarkerInvalid) {
					t.Fatalf("error = %v, want ErrCatalogMarkerInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CatalogPrincipalFromProvenance: %v", err)
			}
			if got != tc.want {
				t.Fatalf("principal = %+v, want %+v", got, tc.want)
			}
		})
	}
}
