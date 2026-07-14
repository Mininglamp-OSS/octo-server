package main

import (
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/notify"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

func TestCardDispatchRegistryInstalledBeforeModuleConstruction(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)
	install := strings.Index(text, "installCardDispatch(ctx)")
	setup := strings.Index(text, "module.Setup(ctx)")
	if install < 0 {
		t.Fatal("main must install the per-context card dispatch registry")
	}
	if setup < 0 || install > setup {
		t.Fatal("card dispatch registry must be installed before module.Setup constructs producers")
	}
}

// TestNotificationCardProducerRegistrations pins the production card-dispatch
// producers to the reviewed shape (Sender identity, DM-only, octo/v1,
// system-notification Space policy, MaxInFlight=20). Both `summary-notify` and
// `docs-notify` share this shape by design (see modules/notify — capability
// isolation lives on the producer ID, not the sender identity). Any *new*
// production producer needs its own reviewed entry here, so this test
// deliberately fails on unknown IDs — the sender-of-cards allowlist is not
// silently extensible.
func TestNotificationCardProducerRegistrations(t *testing.T) {
	specs := cardDispatchProducerSpecs()
	want := map[carddispatch.ProducerID]bool{
		"summary-notify": false,
		"docs-notify":    false,
	}
	for _, spec := range specs {
		seen, known := want[spec.ID]
		if !known {
			t.Fatalf("unexpected production producer %q — new senders require a reviewed entry", spec.ID)
		}
		if seen {
			t.Fatalf("producer %q registered twice", spec.ID)
		}
		want[spec.ID] = true

		if !spec.Enabled {
			t.Fatalf("%s producer must be enabled", spec.ID)
		}
		if spec.SenderUID != notify.NotifyBotUIDValue {
			t.Fatalf("%s sender UID = %q; want existing notification bot %q", spec.ID, spec.SenderUID, notify.NotifyBotUIDValue)
		}
		if len(spec.AllowedChannelTypes) != 1 || spec.AllowedChannelTypes[0] != common.ChannelTypePerson.Uint8() {
			t.Fatalf("%s allowed channel types = %v; want DM only", spec.ID, spec.AllowedChannelTypes)
		}
		if len(spec.AllowedProfiles) != 1 || spec.AllowedProfiles[0] != cardmsg.ProfileV1 {
			t.Fatalf("%s allowed profiles = %v; want octo/v1 only", spec.ID, spec.AllowedProfiles)
		}
		if spec.SpacePolicy != carddispatch.SpacePolicySystemNotification {
			t.Fatalf("%s space policy = %q; want system_notification", spec.ID, spec.SpacePolicy)
		}
		if spec.GroupPolicy != carddispatch.GroupPolicyMemberRequired {
			t.Fatalf("%s group policy = %q; want member_required", spec.ID, spec.GroupPolicy)
		}
		if spec.MaxInFlight != 20 {
			t.Fatalf("%s max in flight = %d; want 20", spec.ID, spec.MaxInFlight)
		}
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("expected producer %q was not registered", id)
		}
	}
}
