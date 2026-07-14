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

func TestSummaryNotifyIsOnlyProductionCardProducer(t *testing.T) {
	specs := summaryNotifyProducerSpecs()
	if len(specs) != 1 {
		t.Fatalf("production registry has %d producers; want summary-notify only", len(specs))
	}

	spec := specs[0]
	if spec.ID != carddispatch.ProducerID("summary-notify") {
		t.Fatalf("producer ID = %q; want summary-notify", spec.ID)
	}
	if !spec.Enabled {
		t.Fatal("summary-notify producer must be enabled")
	}
	if spec.SenderUID != notify.SummaryBotUIDValue {
		t.Fatalf("sender UID = %q; want %q", spec.SenderUID, notify.SummaryBotUIDValue)
	}
	if len(spec.AllowedChannelTypes) != 1 || spec.AllowedChannelTypes[0] != common.ChannelTypePerson.Uint8() {
		t.Fatalf("allowed channel types = %v; want DM only", spec.AllowedChannelTypes)
	}
	if len(spec.AllowedProfiles) != 1 || spec.AllowedProfiles[0] != cardmsg.ProfileV1 {
		t.Fatalf("allowed profiles = %v; want octo/v1 only", spec.AllowedProfiles)
	}
	if spec.SpacePolicy != carddispatch.SpacePolicySystemNotification {
		t.Fatalf("space policy = %q; want system_notification", spec.SpacePolicy)
	}
	if spec.GroupPolicy != carddispatch.GroupPolicyMemberRequired {
		t.Fatalf("group policy = %q; want member_required", spec.GroupPolicy)
	}
	if spec.MaxInFlight != 20 {
		t.Fatalf("max in flight = %d; want 20", spec.MaxInFlight)
	}
}
