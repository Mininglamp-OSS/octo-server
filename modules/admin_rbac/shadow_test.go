package adminrbac

import (
	"errors"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type fakeEffectivePermissionProvider struct {
	result EffectivePermissions
	err    error
	calls  int
}

func (f *fakeEffectivePermissionProvider) EffectivePermissions(string) (EffectivePermissions, error) {
	f.calls++
	return f.result, f.err
}

type collectingShadowSink struct {
	events []ShadowEvent
}

func (s *collectingShadowSink) Observe(event ShadowEvent) {
	s.events = append(s.events, event)
}

func TestShadowObserverClassifiesMappingAndRBACOutcomes(t *testing.T) {
	tests := []struct {
		name          string
		provider      *fakeEffectivePermissionProvider
		operationID   string
		legacyAllowed bool
		wantOutcome   string
		wantError     string
	}{
		{
			name:          "match",
			provider:      &fakeEffectivePermissionProvider{result: EffectivePermissions{UID: "u1", Permissions: []string{"workplace.app.read"}}},
			operationID:   "workplace.app.list",
			legacyAllowed: true,
			wantOutcome:   ShadowOutcomeMatch,
		},
		{
			name:          "legacy allows rbac denies",
			provider:      &fakeEffectivePermissionProvider{result: EffectivePermissions{UID: "u1"}},
			operationID:   "workplace.app.list",
			legacyAllowed: true,
			wantOutcome:   ShadowOutcomeLegacyAllowRBACDeny,
		},
		{
			name:          "legacy denies rbac allows",
			provider:      &fakeEffectivePermissionProvider{result: EffectivePermissions{UID: "u1", Permissions: []string{"workplace.category_app.reorder"}}},
			operationID:   "workplace.category_app.reorder",
			legacyAllowed: false,
			wantOutcome:   ShadowOutcomeLegacyDenyRBACAllow,
		},
		{
			name:          "rbac evaluation error",
			provider:      &fakeEffectivePermissionProvider{err: errors.New("database unavailable")},
			operationID:   "workplace.app.list",
			legacyAllowed: true,
			wantOutcome:   ShadowOutcomeRBACEvaluationError,
			wantError:     ShadowErrorRBACEvaluation,
		},
		{
			name:          "unknown operation",
			provider:      &fakeEffectivePermissionProvider{},
			operationID:   "workplace.unknown",
			legacyAllowed: true,
			wantOutcome:   ShadowOutcomeMappingError,
			wantError:     ShadowErrorMapping,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &collectingShadowSink{}
			observer := NewShadowObserver(tt.provider, func() bool { return true }, sink)
			observer.Observe("u1", tt.operationID, tt.legacyAllowed)
			if len(sink.events) != 1 {
				t.Fatalf("events = %d, want 1", len(sink.events))
			}
			event := sink.events[0]
			if event.Outcome != tt.wantOutcome || event.ErrorKind != tt.wantError {
				t.Fatalf("event = %#v, want outcome=%q error=%q", event, tt.wantOutcome, tt.wantError)
			}
			if tt.wantError == "" && event.Permission == "" {
				t.Fatal("successful event has no generated permission")
			}
			if tt.provider.calls != 1 && tt.wantError == "" {
				t.Fatalf("provider calls = %d, want 1", tt.provider.calls)
			}
			if tt.wantError == ShadowErrorMapping && tt.provider.calls != 0 {
				t.Fatalf("mapping error called provider %d times, want 0", tt.provider.calls)
			}
		})
	}
}

func TestShadowObserverDisabledSkipsEvaluationAndObservation(t *testing.T) {
	provider := &fakeEffectivePermissionProvider{result: EffectivePermissions{UID: "u1", Permissions: []string{"workplace.app.read"}}}
	sink := &collectingShadowSink{}
	observer := NewShadowObserver(provider, func() bool { return false }, sink)
	observer.Observe("u1", "workplace.app.list", true)
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0 while disabled", provider.calls)
	}
	if len(sink.events) != 0 {
		t.Fatalf("events = %d, want 0 while disabled", len(sink.events))
	}
}

func TestWorkplaceShadowEnabledDefaultsOffAndAcceptsExplicitValues(t *testing.T) {
	for _, value := range []string{"", "0", "false", "off", "invalid"} {
		t.Setenv(WorkplaceShadowEnv, value)
		if WorkplaceShadowEnabled() {
			t.Errorf("WorkplaceShadowEnabled() = true for %q, want false", value)
		}
	}
	for _, value := range []string{"1", "true", "yes", "on"} {
		t.Setenv(WorkplaceShadowEnv, value)
		if !WorkplaceShadowEnabled() {
			t.Errorf("WorkplaceShadowEnabled() = false for %q, want true", value)
		}
	}
}

func TestShadowObserverDoesNotAcceptResourceScope(t *testing.T) {
	provider := &fakeEffectivePermissionProvider{result: EffectivePermissions{UID: "u1", Permissions: []string{"workplace.app.read"}}}
	sink := &collectingShadowSink{}
	observer := NewShadowObserver(provider, func() bool { return true }, sink)
	observer.Observe("u1", "workplace.app.list", true)
	if got := sink.events[0].Permission; got != "workplace.app.read" {
		t.Fatalf("permission = %q, want generated global permission", got)
	}
}

func TestWorkplaceRoleMatrixKeepsWriteAndReorderPermissionsIndependent(t *testing.T) {
	adminCapable, err := Evaluate("admin", []RoleSnapshot{{
		RoleKey:     "admin-capable",
		Status:      activeStatus,
		Permissions: []string{"workplace.app.read", "workplace.category_app.reorder"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	superAdminCapable, err := Evaluate("super-admin", []RoleSnapshot{{
		RoleKey: "super-admin-capable",
		Status:  activeStatus,
		Permissions: []string{
			"workplace.app.read", "workplace.app.write",
			"workplace.category.read", "workplace.category.write",
			"workplace.banner.read", "workplace.banner.write",
			"workplace.category_app.reorder",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	noPermission, err := Evaluate("none", nil)
	if err != nil {
		t.Fatal(err)
	}

	assertPermission := func(name string, result EffectivePermissions, permission string, want bool) {
		t.Helper()
		got, err := Allows(result, permission, "", "", "", "")
		if err != nil {
			t.Fatalf("%s Allows(%q): %v", name, permission, err)
		}
		if got != want {
			t.Errorf("%s Allows(%q) = %v, want %v", name, permission, got, want)
		}
	}
	assertPermission("admin", adminCapable, "workplace.category_app.reorder", true)
	assertPermission("admin", adminCapable, "workplace.app.write", false)
	assertPermission("superAdmin", superAdminCapable, "workplace.app.write", true)
	assertPermission("superAdmin", superAdminCapable, "workplace.category_app.reorder", true)
	assertPermission("none", noPermission, "workplace.app.read", false)
}

func TestShadowObserverCoversAllDeclaredWorkplaceOperations(t *testing.T) {
	provider := &fakeEffectivePermissionProvider{result: EffectivePermissions{UID: "u1", Permissions: []string{
		"workplace.app.read", "workplace.app.write",
		"workplace.banner.read", "workplace.banner.write",
		"workplace.category.read", "workplace.category.write",
		"workplace.category_app.reorder",
	}}}
	sink := &collectingShadowSink{}
	observer := NewShadowObserver(provider, func() bool { return true }, sink)
	operationIDs := []string{
		"workplace.category.create", "workplace.category.list", "workplace.category.reorder", "workplace.category.delete", "workplace.category.update",
		"workplace.category_app.list", "workplace.category_app.reorder", "workplace.category_app.create", "workplace.category_app.delete",
		"workplace.app.create", "workplace.app.list", "workplace.app.update", "workplace.app.delete",
		"workplace.banner.create", "workplace.banner.list", "workplace.banner.delete", "workplace.banner.update", "workplace.banner.reorder",
	}
	for _, operationID := range operationIDs {
		observer.Observe("u1", operationID, true)
	}
	if len(sink.events) != 18 {
		t.Fatalf("events = %d, want 18", len(sink.events))
	}
	for _, event := range sink.events {
		if event.Outcome != ShadowOutcomeMatch || event.Permission == "" {
			t.Errorf("workplace operation produced unexpected event: %#v", event)
		}
	}
}

func TestShadowObserverIgnoresNonWorkplaceOperations(t *testing.T) {
	provider := &fakeEffectivePermissionProvider{result: EffectivePermissions{UID: "u1", Permissions: []string{"group.read", "space.read", "robot.read"}}}
	sink := &collectingShadowSink{}
	observer := NewShadowObserver(provider, func() bool { return true }, sink)
	for _, operationID := range []string{"group.read", "space.read", "robot.read", "common.app_config.get", "unknown.module.operation"} {
		observer.Observe("u1", operationID, true)
	}
	if provider.calls != 0 || len(sink.events) != 0 {
		t.Fatalf("non-workplace observations = calls:%d events:%d, want zero", provider.calls, len(sink.events))
	}
}

func TestZapShadowSinkEmitsOnlyAuthorizationComparisonFields(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	previous := zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(previous)

	(zapShadowSink{}).Observe(ShadowEvent{
		UID:           "u1",
		OperationID:   "workplace.app.list",
		Permission:    "workplace.app.read",
		LegacyAllowed: true,
		RBACAllowed:   false,
		Outcome:       ShadowOutcomeLegacyAllowRBACDeny,
	})
	if logs.Len() != 1 {
		t.Fatalf("log entries = %d, want 1", logs.Len())
	}
	fields := logs.All()[0].ContextMap()
	for _, key := range []string{"uid", "operation_id", "permission", "legacy_allowed", "rbac_allowed", "outcome"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("log field %q is missing: %#v", key, fields)
		}
	}
	for _, forbidden := range []string{"token", "request_body", "resource_id", "group_no", "space_id", "robot_id", "member_uid", "audit_id"} {
		if _, ok := fields[forbidden]; ok {
			t.Errorf("sensitive or resource field %q was emitted: %#v", forbidden, fields)
		}
	}
}
