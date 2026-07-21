package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
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
	actionInstall := strings.Index(text, "installCardActionDispatch(ctx)")
	if actionInstall < 0 || actionInstall > setup {
		t.Fatal("card action dispatch service must be installed before module.Setup constructs message handlers")
	}
	start := strings.Index(text, "cardActionRuntime.Start(")
	stop := strings.LastIndex(text, "cardActionRuntime.Stop()")
	serve := strings.Index(text, "svc.Run(s)")
	if start < setup || start > serve {
		t.Fatal("card action dispatcher must start after module setup and before serving")
	}
	if stop < serve {
		t.Fatal("card action dispatcher must stop after the server exits")
	}
	if !strings.Contains(text, "notify.NewActionFinalizerFromContext(ctx)") {
		t.Fatal("card action dispatcher must compose specialized finalizers with the standard fallback")
	}
	if strings.Contains(text, "notify.NewDocsActionFinalizerFromContext(ctx)") {
		t.Fatal("composition root must not wire the docs-only finalizer directly")
	}
}

// TestNotificationCardProducerRegistrations pins the production card-dispatch
// producers to the reviewed shape (Sender identity, DM-only, octo/v1,
// system-notification Space policy, MaxInFlight=20). `summary-notify`,
// `docs-notify`, and the internal-only `action-outcome` sender share this shape
// by design (see modules/notify — capability isolation lives on the producer
// ID, not the sender identity). Any *new*
// production producer needs its own reviewed entry here, so this test
// deliberately fails on unknown IDs — the sender-of-cards allowlist is not
// silently extensible.
func TestNotificationCardProducerRegistrations(t *testing.T) {
	t.Setenv("OCTO_DOCS_APPROVAL_CARD_ENABLED", "false")
	specs := cardDispatchProducerSpecs()
	want := map[carddispatch.ProducerID]bool{
		"summary-notify": false,
		"docs-notify":    false,
		"action-outcome": false,
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

func TestDocsApprovalProducerEnablesOnlyReviewedV2Owner(t *testing.T) {
	t.Setenv("OCTO_DOCS_APPROVAL_CARD_ENABLED", "true")
	specs := cardDispatchProducerSpecs()
	for _, spec := range specs {
		if spec.ID != "docs-notify" {
			if spec.ActionEventOwner != "" {
				t.Fatalf("non-docs producer %q unexpectedly owns actions", spec.ID)
			}
			continue
		}
		if spec.ActionEventOwner != "docs" {
			t.Fatalf("docs action owner = %q, want docs", spec.ActionEventOwner)
		}
		if len(spec.AllowedProfiles) != 2 || spec.AllowedProfiles[0] != cardmsg.ProfileV1 || spec.AllowedProfiles[1] != cardmsg.ProfileV2 {
			t.Fatalf("docs profiles = %v, want [octo/v1 octo/v2]", spec.AllowedProfiles)
		}
		return
	}
	t.Fatal("docs-notify producer missing")
}

func TestConfiguredApprovalRouteAddsOwnerBoundV2Producer(t *testing.T) {
	specs, err := cardActionApprovalProducerSpecs([]cardactiondispatch.RouteSpec{
		{
			SenderUID: "notification", Owner: "smart-summary", ActionType: "summary.publish.decision",
			NotifyTokenEnv: "OCTO_SMART_SUMMARY_NOTIFY_TOKEN",
		},
		{
			SenderUID: "notification", Owner: "smart-summary", ActionType: "summary.delete.decision",
			NotifyTokenEnv: "OCTO_SMART_SUMMARY_NOTIFY_TOKEN",
		},
	})
	if err != nil {
		t.Fatalf("cardActionApprovalProducerSpecs() error = %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("producer count = %d, want 1 per sender/owner", len(specs))
	}
	spec := specs[0]
	if spec.SenderUID != notify.NotifyBotUIDValue || spec.ActionEventOwner != "smart-summary" {
		t.Fatalf("producer = %+v", spec)
	}
	if len(spec.AllowedProfiles) != 1 || spec.AllowedProfiles[0] != cardmsg.ProfileV2 {
		t.Fatalf("profiles = %v, want [octo/v2]", spec.AllowedProfiles)
	}
}

// TestCardActionDispatchInertWhenCardsDisabled pins the rollback story from the
// runbook ("global OCTO_CARD_MESSAGE_ENABLED=false"): with the deployment gate
// off, notify routes may stay in the config without crashing startup. The gate
// is a master kill switch — the card_action ingress and the notify/approval send
// paths already refuse cards when it is off, so the dispatch worker has no work
// and must be skipped rather than demanded. Before the fix this panicked with
// "action notify routes require OCTO_CARD_MESSAGE_ENABLED".
//
// The disabled branch never touches the *config.Context (Redis/worker
// construction only happens on the enabled path), so a nil ctx exercises it
// without standing up MySQL/Redis/WuKongIM.
func TestCardActionDispatchInertWhenCardsDisabled(t *testing.T) {
	t.Setenv("OCTO_CARD_MESSAGE_ENABLED", "false")
	t.Setenv("OCTO_DOCS_APPROVAL_CARD_ENABLED", "false")
	t.Setenv("OCTO_TASKS_CARD_ACTION_SECRET", strings.Repeat("a", 32))
	t.Setenv("OCTO_TASKS_NOTIFY_TOKEN", strings.Repeat("b", 32))
	t.Setenv("OCTO_CARD_ACTION_ROUTES", `[{"sender_uid":"notification","owner":"tasks","action_type":"task.execute.decision","url":"https://tasks.internal/v1/card-actions/decide","secret_env":"OCTO_TASKS_CARD_ACTION_SECRET","notify_token_env":"OCTO_TASKS_NOTIFY_TOKEN"}]`)

	runtime, err := installCardActionDispatch(nil)
	if err != nil {
		t.Fatalf("installCardActionDispatch() with cards disabled must not error, got %v", err)
	}
	if runtime == nil {
		t.Fatal("installCardActionDispatch() returned a nil runtime; boot dereferences it")
	}
	if runtime.dispatcher != nil {
		t.Fatal("disabled gate must not construct a dispatch worker")
	}
	if runtime.redisClient != nil {
		t.Fatal("disabled gate must not construct a redis consumer")
	}
	// Start/Stop must be safe no-ops so main's boot sequence does not panic.
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("inert runtime Start() must be a no-op, got %v", err)
	}
	runtime.Stop()
}

// TestCardActionDispatchInertWhenGateUnsetWithDocsApproval covers the two other
// documented gate-off inputs the first test does not: an UNSET gate (not just
// "false") and OCTO_DOCS_APPROVAL_CARD_ENABLED=true. Both formerly panicked
// ("action notify routes require ..." / "OCTO_DOCS_APPROVAL_CARD_ENABLED
// requires ..."); with the kill switch they must boot inert instead.
func TestCardActionDispatchInertWhenGateUnsetWithDocsApproval(t *testing.T) {
	t.Setenv("OCTO_CARD_MESSAGE_ENABLED", "") // unset ⇒ gate off (not the literal "false")
	t.Setenv("OCTO_DOCS_APPROVAL_CARD_ENABLED", "true")
	t.Setenv("OCTO_TASKS_CARD_ACTION_SECRET", strings.Repeat("a", 32))
	t.Setenv("OCTO_TASKS_NOTIFY_TOKEN", strings.Repeat("b", 32))
	t.Setenv("OCTO_CARD_ACTION_ROUTES", `[{"sender_uid":"notification","owner":"tasks","action_type":"task.execute.decision","url":"https://tasks.internal/v1/card-actions/decide","secret_env":"OCTO_TASKS_CARD_ACTION_SECRET","notify_token_env":"OCTO_TASKS_NOTIFY_TOKEN"}]`)

	runtime, err := installCardActionDispatch(nil)
	if err != nil {
		t.Fatalf("unset gate + docs-approval on must boot inert, got error %v", err)
	}
	if runtime == nil || runtime.dispatcher != nil || runtime.redisClient != nil {
		t.Fatalf("expected an inert runtime (no dispatcher/redis), got %+v", runtime)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("inert runtime Start() must be a no-op, got %v", err)
	}
	runtime.Stop()
}

// TestCardActionDispatchValidatesRoutesWhenGateOff pins the validate-before-inert
// ordering: the gate is a kill switch for enablement, NOT a bypass for config
// validation. With the gate off, a genuinely malformed route (here: a callback
// secret < 32 bytes) must still fail startup rather than boot inert.
func TestCardActionDispatchValidatesRoutesWhenGateOff(t *testing.T) {
	t.Setenv("OCTO_CARD_MESSAGE_ENABLED", "false")
	t.Setenv("OCTO_DOCS_APPROVAL_CARD_ENABLED", "false")
	t.Setenv("OCTO_TASKS_CARD_ACTION_SECRET", "too-short") // < 32 bytes ⇒ NewRegistry rejects
	t.Setenv("OCTO_CARD_ACTION_ROUTES", `[{"sender_uid":"notification","owner":"tasks","action_type":"task.execute.decision","url":"https://tasks.internal/v1/card-actions/decide","secret_env":"OCTO_TASKS_CARD_ACTION_SECRET"}]`)

	runtime, err := installCardActionDispatch(nil)
	if err == nil {
		t.Fatal("malformed route config must fail startup even with the gate off (validate-before-inert)")
	}
	if runtime != nil {
		t.Fatalf("expected a nil runtime on validation error, got %+v", runtime)
	}
}

func TestConfiguredApprovalRouteRejectsNonNotificationSender(t *testing.T) {
	_, err := cardActionApprovalProducerSpecs([]cardactiondispatch.RouteSpec{
		{
			SenderUID: "service-bot", Owner: "tasks", ActionType: "task.decision",
			NotifyTokenEnv: "OCTO_TASKS_NOTIFY_TOKEN",
		},
	})
	if err == nil {
		t.Fatal("cardActionApprovalProducerSpecs() error = nil")
	}
}

// TestDeprecatedAllowedURLsEnvIsIgnoredWithStructuredWarn locks the
// http-actions follow-up decision (b): OCTO_CARD_ACTION_ALLOWED_URLS must be
// treated as deprecated. Startup must not fail on it (rolling upgrades still
// carry the variable) and must emit a single structured WARN so operators can
// see it in the ConfigMap review.
func TestDeprecatedAllowedURLsEnvIsIgnoredWithStructuredWarn(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)
	if !strings.Contains(text, `os.Getenv("OCTO_CARD_ACTION_ALLOWED_URLS")`) {
		t.Fatal("main.go no longer reads OCTO_CARD_ACTION_ALLOWED_URLS; expected a deprecation branch")
	}
	// The WARN must be emitted through the shared log helper (log.Warn) so it
	// lands in the standard structured logger pipeline rather than stderr.
	if !strings.Contains(text, "log.Warn(\"OCTO_CARD_ACTION_ALLOWED_URLS is deprecated") {
		t.Fatal("expected log.Warn call announcing the deprecation on startup")
	}
	// Include a machine-readable deprecated_env field so log ingestion can
	// route/alert on it without regex-parsing the message string.
	if !strings.Contains(text, `zap.String("deprecated_env", "OCTO_CARD_ACTION_ALLOWED_URLS")`) {
		t.Fatal("deprecation WARN must include a deprecated_env structured field")
	}
	// The variable must no longer feed into NewRegistry — the whole point of
	// the follow-up is to keep it out of the routing decision.
	if strings.Contains(text, "LoadAllowedURLs(") {
		t.Fatal("LoadAllowedURLs is deleted; main.go must not reference it")
	}
	if !strings.Contains(text, "cardactiondispatch.NewRegistry(specs, os.Getenv)") {
		t.Fatal("NewRegistry must be called with the two-argument (specs, getenv) signature")
	}
}
