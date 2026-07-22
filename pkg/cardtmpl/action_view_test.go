package cardtmpl_test

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

func TestRegistryActionViewResolvesDeclaredSubmitOnly(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()

	view, err := r.ActionView(docsaccessrequest.TemplateID, "0.2.0", cardtmpl.DocsApproveActionID)
	if err != nil || view != "pending" {
		t.Fatalf("ActionView(0.2 approve) = (%q, %v)", view, err)
	}
	if _, err := r.ActionView(docsaccessrequest.TemplateID, "0.3.0", "view_document"); !errors.Is(err, cardtmpl.ErrActionUnknown) {
		t.Fatalf("ActionView(OpenUrl) error = %v, want ErrActionUnknown", err)
	}
	if _, err := r.ActionView(docsaccessrequest.TemplateID, "0.3.0", "missing"); !errors.Is(err, cardtmpl.ErrActionUnknown) {
		t.Fatalf("ActionView(missing) error = %v, want ErrActionUnknown", err)
	}
}
