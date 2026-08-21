package authz

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestRepositoryPermissionContract(t *testing.T) {
	root := repositoryRoot(t)
	manifest, err := LoadManifest(filepath.Join(root, "authz", "manager-permissions.yaml"))
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest() error = %v", err)
	}
	if err := ValidateCriticalPermissions(manifest); err != nil {
		t.Fatalf("ValidateCriticalPermissions() error = %v", err)
	}
	if err := ValidateRecognizedGateLocations(root); err != nil {
		t.Fatalf("ValidateRecognizedGateLocations() error = %v", err)
	}
	scanned, err := ScanDirectGates(root)
	if err != nil {
		t.Fatalf("ScanDirectGates() error = %v", err)
	}
	routes, err := ScanManagerRoutes(root, scanned)
	if err != nil {
		t.Fatalf("ScanManagerRoutes() error = %v", err)
	}
	platformGates, err := PlatformGates(scanned, routes)
	if err != nil {
		t.Fatalf("PlatformGates() error = %v", err)
	}
	if err := ValidateGateInventory(platformGates, manifest.GateSites); err != nil {
		t.Fatalf("ValidateGateInventory() error = %v", err)
	}
	exclusions := append(ManagerRouteBoundaryExclusions(), ManagerRBACMetaSurfaceExclusions()...)
	if err := ValidateRouteCoverage(routes, manifest.Operations, exclusions); err != nil {
		t.Fatalf("ValidateRouteCoverage() error = %v", err)
	}
	if got, want := len(manifest.Permissions), 90; got != want {
		t.Fatalf("permission count = %d, want %d", got, want)
	}
	if got, want := len(manifest.LegacyCapabilities), 20; got != want {
		t.Fatalf("legacy capability count = %d, want %d", got, want)
	}
	if got, want := len(manifest.Operations), 130; got != want {
		t.Fatalf("global operation count = %d, want %d", got, want)
	}
	if got, want := len(routes), len(manifest.Operations)+len(exclusions); got != want {
		t.Fatalf("source platform route count = %d, want %d global operations and meta exclusions", got, want)
	}
	assertSourceRouteBoundaries(t, routes)
	if got, want := len(platformGates), 96; got != want {
		t.Fatalf("platform gate count = %d, want %d", got, want)
	}
	if got, want := len(manifest.GateSites), 96; got != want {
		t.Fatalf("manifest gate count = %d, want %d", got, want)
	}
	if got, want := len(scanned), 103; got != want {
		t.Fatalf("recognized source gate count = %d, want %d including RBAC meta gates and 5 excluded business gates", got, want)
	}
	moduleCounts := make(map[string]int)
	for _, gate := range platformGates {
		moduleCounts[gate.Module]++
	}
	for module, want := range map[string]int{"workplace": 18, "message": 10, "app_bot": 9, "robot": 8} {
		if got := moduleCounts[module]; got != want {
			t.Errorf("%s direct gate count = %d, want %d", module, got, want)
		}
	}
	gateKinds := make(map[LegacyGate]int)
	for _, gate := range platformGates {
		gateKinds[gate.LegacyGate]++
	}
	if gateKinds[LegacyGateAdmin]+gateKinds[LegacyGateSuperAdmin] != 95 || gateKinds[LegacyGateDashboardReadPolicy] != 1 {
		t.Fatalf("platform gate kinds = %#v, want 95 direct legacy and 1 dashboard-read-policy gate", gateKinds)
	}
	assertGlobalOperationBoundary(t, manifest)
	assertSensitiveTaxonomy(t, manifest)
	assertLegacyCapabilitySemantics(t, manifest)
	assertProductionDoesNotConsumeRegistry(t, root)
}

func assertSourceRouteBoundaries(t *testing.T, routes []ScannedRoute) {
	t.Helper()
	routeByKey := make(map[string]ScannedRoute, len(routes))
	platformAppBot := 0
	for _, route := range routes {
		routeByKey[httpRouteKey(route.Method, route.Path)] = route
		if strings.HasPrefix(route.Path, "/v1/admin/app_bot") {
			platformAppBot++
		}
		if strings.HasPrefix(route.Path, "/v1/groups/") || strings.HasPrefix(route.Path, "/v1/space/") ||
			strings.HasPrefix(route.Path, "/v1/statistics/") || strings.HasPrefix(route.Path, "/v1/manager/secrets/") {
			t.Errorf("non-platform route entered source inventory: %s %s", route.Method, route.Path)
		}
	}
	if platformAppBot != 9 {
		t.Fatalf("platform App Bot gated source routes = %d, want 9", platformAppBot)
	}
	avatar, ok := routeByKey[httpRouteKey("POST", "/v1/users/:uid/avatar")]
	if !ok || avatar.Handler != "User.uploadAvatar" || len(avatar.GateSites) != 1 || avatar.GateSites[0] != "modules/user/api.go::User.uploadAvatar#1" {
		t.Errorf("platform App Bot avatar route = %#v, want only User.uploadAvatar#1", avatar)
	}
}

func assertGlobalOperationBoundary(t *testing.T, manifest *Manifest) {
	t.Helper()
	operationByID := make(map[string]Operation, len(manifest.Operations))
	platformAppBotCount := 0
	for _, operation := range manifest.Operations {
		operationByID[operation.ID] = operation
		if operation.Path == "/v1/manager/me" {
			t.Errorf("legacy manager capability projection entered global operation inventory: %s", operation.ID)
		}
		if strings.HasPrefix(operation.Path, "/v1/space/") {
			t.Errorf("pure Space ACL route entered global contract: %s", operation.Path)
		}
		if strings.HasPrefix(operation.Path, "/v1/robot/") {
			t.Errorf("pure Robot business route entered global contract: %s", operation.Path)
		}
		if strings.HasPrefix(operation.Path, "/v1/groups/") {
			t.Errorf("pure Group ACL route entered global contract: %s", operation.Path)
		}
		if strings.HasPrefix(operation.Path, "/v1/statistics/") || strings.HasPrefix(operation.Path, "/v1/manager/secrets/") {
			t.Errorf("excluded route entered global contract: %s", operation.Path)
		}
		if strings.HasPrefix(operation.Path, "/v1/admin/app_bot") {
			platformAppBotCount++
		}
	}
	if platformAppBotCount != 9 {
		t.Errorf("platform App Bot operation count = %d, want 9", platformAppBotCount)
	}
	for _, id := range []string{"group.member.remove", "group.member.remove_legacy", "statistics.overview", "statistics.user_registration", "statistics.group_creation"} {
		if _, exists := operationByID[id]; exists {
			t.Errorf("excluded operation %q remains in global inventory", id)
		}
	}
	avatar := operationByID["app_bot.avatar.update"]
	if avatar.Scope != ScopeGlobalAdminWithBusinessACL || avatar.BusinessACL == nil {
		t.Errorf("operation app_bot.avatar.update must declare business ACL fallback metadata")
	}
	if operationByID["app_bot.token.reveal"].Scope != ScopeGlobalAdmin {
		t.Errorf("platform token reveal must not inherit Space ACL scope")
	}
	if got := operationByID["group.member.force_remove"].Permission; got != "group.member.remove" {
		t.Errorf("manager force-remove permission = %q, want group.member.remove", got)
	}
	if avatar.BusinessACL == nil || avatar.BusinessACL.Type != "self_or_user_bot_creator_or_bot_owner_or_space_admin" ||
		!strings.Contains(avatar.BusinessACL.Description, "human self-service") || !strings.Contains(avatar.BusinessACL.Description, "User Bot creator") {
		t.Errorf("App Bot avatar business ACL fallback is incomplete: %#v", avatar.BusinessACL)
	}
}

func assertSensitiveTaxonomy(t *testing.T, manifest *Manifest) {
	t.Helper()
	operationByID := make(map[string]Operation, len(manifest.Operations))
	for _, operation := range manifest.Operations {
		operationByID[operation.ID] = operation
	}
	want := map[string]string{
		"user.password.reset":                 "user.password.reset",
		"user.admin.create":                   "user.admin.manage",
		"backup.download":                     "backup.download",
		"message.direct_history.list":         "message.direct_history.read",
		"app_bot.token.reveal":                "app_bot.token.reveal",
		"common.system_setting.update":        "system_setting.write",
		"space.destroy":                       "space.destroy",
		"dashboard.direct_chat_activity.read": "dashboard.direct_chat_activity.read",
		"workplace.category_app.reorder":      "workplace.category_app.reorder",
	}
	for operationID, permission := range want {
		if got := operationByID[operationID].Permission; got != permission {
			t.Errorf("operation %q permission = %q, want %q", operationID, got, permission)
		}
	}
	pairs := [][2]string{
		{"user.list", "user.password.reset"},
		{"backup.history.list", "backup.download"},
		{"message.history.list", "message.direct_history.list"},
		{"app_bot.detail", "app_bot.token.reveal"},
		{"common.system_setting.list", "common.system_setting.update"},
		{"space.detail", "space.destroy"},
		{"dashboard.overview.read", "dashboard.direct_chat_activity.read"},
	}
	for _, pair := range pairs {
		if operationByID[pair[0]].Permission == operationByID[pair[1]].Permission {
			t.Errorf("risk-distinct operations %q and %q share permission %q", pair[0], pair[1], operationByID[pair[0]].Permission)
		}
	}
	permissionByKey := make(map[string]Permission, len(manifest.Permissions))
	for _, permission := range manifest.Permissions {
		permissionByKey[permission.Key] = permission
	}
	for _, key := range []string{"workplace.category_app.reorder", "workplace.app.write"} {
		if got := permissionByKey[key].Sensitivity; got != SensitivityElevated {
			t.Errorf("permission %q sensitivity = %q, want elevated", key, got)
		}
	}
	gateBySource := make(map[string]GateSite, len(manifest.GateSites))
	for _, gate := range manifest.GateSites {
		gateBySource[gate.Source] = gate
	}
	tiersByPermission := make(map[string]map[LegacyGate]struct{})
	for _, operation := range manifest.Operations {
		if operation.Permission != "workplace.category_app.reorder" && operation.Permission != "workplace.app.write" {
			continue
		}
		if tiersByPermission[operation.Permission] == nil {
			tiersByPermission[operation.Permission] = make(map[LegacyGate]struct{})
		}
		for _, source := range operation.GateSites {
			tiersByPermission[operation.Permission][gateBySource[source].LegacyGate] = struct{}{}
		}
	}
	if tiers := tiersByPermission["workplace.category_app.reorder"]; len(tiers) != 1 {
		t.Errorf("workplace.category_app.reorder gate tiers = %v, want admin only", tiers)
	} else if _, ok := tiers[LegacyGateAdmin]; !ok {
		t.Errorf("workplace.category_app.reorder gate tiers = %v, want admin only", tiers)
	}
	if tiers := tiersByPermission["workplace.app.write"]; len(tiers) != 1 {
		t.Errorf("workplace.app.write gate tiers = %v, want super_admin only", tiers)
	} else if _, ok := tiers[LegacyGateSuperAdmin]; !ok {
		t.Errorf("workplace.app.write gate tiers = %v, want super_admin only", tiers)
	}
}

func assertLegacyCapabilitySemantics(t *testing.T, manifest *Manifest) {
	t.Helper()
	for _, capability := range manifest.LegacyCapabilities {
		if capability.Key != "dashboard.read" {
			continue
		}
		if capability.Mode != AggregateAll {
			t.Fatalf("dashboard.read aggregate mode = %q, want %q", capability.Mode, AggregateAll)
		}
		want := map[string]bool{
			"dashboard.read":                      true,
			"dashboard.direct_chat_activity.read": true,
		}
		if len(capability.Permissions) != len(want) {
			t.Fatalf("dashboard.read permissions = %v, want exactly %v", capability.Permissions, want)
		}
		for _, permission := range capability.Permissions {
			if !want[permission] {
				t.Fatalf("dashboard.read includes unexpected permission %q", permission)
			}
		}
		return
	}
	t.Fatal("missing dashboard.read legacy capability")
}

func assertProductionDoesNotConsumeRegistry(t *testing.T, root string) {
	t.Helper()
	consumers, err := productionRegistryConsumers(root)
	if err != nil {
		t.Fatalf("scan production imports: %v", err)
	}
	var unexpected []string
	for _, consumer := range consumers {
		if !strings.HasPrefix(consumer, "modules/admin_rbac/") {
			unexpected = append(unexpected, consumer)
		}
	}
	if len(unexpected) != 0 {
		t.Errorf("production code outside modules/admin_rbac imports generated permission registry: %v", unexpected)
	}
}

func productionRegistryConsumers(root string) ([]string, error) {
	wantImport := "github.com/Mininglamp-OSS/octo-server/pkg/authz"
	fset := token.NewFileSet()
	var consumers []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			switch relative {
			case ".git", "vendor", "pkg/authz", "tools/manager-permission-gen":
				return fs.SkipDir
			}
			if entry.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			if strings.Trim(imported.Path.Value, "\"") == wantImport {
				consumers = append(consumers, relative)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(consumers)
	return consumers, nil
}

func TestProductionRegistryConsumersScanAllProductionRoots(t *testing.T) {
	root := t.TempDir()
	consumer := "package fixture\nimport _ \"github.com/Mininglamp-OSS/octo-server/pkg/authz\"\n"
	for _, path := range []string{
		"pkg/example/consumer.go",
		"internal/example/consumer.go",
		"cmd/example/consumer.go",
	} {
		writeFixture(t, root, path, consumer)
	}
	writeFixture(t, root, "pkg/example/consumer_test.go", consumer)
	writeFixture(t, root, "pkg/authz/registry.go", "package authz\n")
	writeFixture(t, root, "tools/manager-permission-gen/main.go", consumer)

	got, err := productionRegistryConsumers(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cmd/example/consumer.go",
		"internal/example/consumer.go",
		"pkg/example/consumer.go",
	}
	if len(got) != len(want) {
		t.Fatalf("production registry consumers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("production registry consumers = %v, want %v", got, want)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	entries, err := filepath.Glob(filepath.Join(root, "go.mod"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("repository root %q does not contain go.mod", root)
	}
	return root
}
