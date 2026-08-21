package authz

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
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
	scanned, err := ScanDirectGates(root)
	if err != nil {
		t.Fatalf("ScanDirectGates() error = %v", err)
	}
	if err := ValidateGateInventory(scanned, manifest.GateSites); err != nil {
		t.Fatalf("ValidateGateInventory() error = %v", err)
	}
	routes, err := ScanManagerRoutes(root, scanned)
	if err != nil {
		t.Fatalf("ScanManagerRoutes() error = %v", err)
	}
	if err := ValidateRouteCoverage(routes, manifest.Operations, ManagerRouteBoundaryExclusions()); err != nil {
		t.Fatalf("ValidateRouteCoverage() error = %v", err)
	}
	if got, want := len(manifest.Operations), 129; got != want {
		t.Fatalf("global operation count = %d, want %d", got, want)
	}
	if got, want := len(routes), len(manifest.Operations)+len(ManagerRouteBoundaryExclusions()); got != want {
		t.Fatalf("source gated route count = %d, want %d global operations + %d boundary exclusions", got, len(manifest.Operations), len(ManagerRouteBoundaryExclusions()))
	}
	assertSourceRouteBoundaries(t, routes)
	if got, want := len(scanned), 100; got != want {
		t.Fatalf("direct gate count = %d, want %d", got, want)
	}
	files := make(map[string]struct{})
	moduleCounts := make(map[string]int)
	for _, gate := range scanned {
		files[sourceFile(gate.Source)] = struct{}{}
		moduleCounts[gate.Module]++
	}
	if got, want := len(files), 18; got != want {
		t.Fatalf("direct gate file count = %d, want %d", got, want)
	}
	for module, want := range map[string]int{"workplace": 18, "message": 10, "app_bot": 9, "robot": 8} {
		if got := moduleCounts[module]; got != want {
			t.Errorf("%s direct gate count = %d, want %d", module, got, want)
		}
	}
	assertGlobalOperationBoundary(t, manifest)
	assertSensitiveTaxonomy(t, manifest)
	assertProductionDoesNotConsumeRegistry(t, root)
}

func assertSourceRouteBoundaries(t *testing.T, routes []ScannedRoute) {
	t.Helper()
	routeByKey := make(map[string]ScannedRoute, len(routes))
	platformAppBot := 0
	spaceAppBot := 0
	for _, route := range routes {
		routeByKey[httpRouteKey(route.Method, route.Path)] = route
		if strings.HasPrefix(route.Path, "/v1/admin/app_bot") {
			platformAppBot++
		}
		if strings.HasPrefix(route.Path, "/v1/space/:space_id/app_bot/") {
			spaceAppBot++
		}
	}
	if platformAppBot != 9 || spaceAppBot != 7 {
		t.Fatalf("App Bot gated source routes: platform=%d space-boundary=%d, want 9 and 7", platformAppBot, spaceAppBot)
	}
	for key, handler := range map[string]string{
		httpRouteKey("DELETE", "/v1/groups/:group_no/members"):      "Group.memberRemove",
		httpRouteKey("POST", "/v1/groups/:group_no/members_delete"): "Group.memberRemove",
	} {
		if route, ok := routeByKey[key]; !ok || route.Handler != handler {
			t.Errorf("mixed Group route %q = %#v, want handler %s", key, route, handler)
		}
	}
	avatar, ok := routeByKey[httpRouteKey("POST", "/v1/users/:uid/avatar")]
	if !ok || avatar.Handler != "User.uploadAvatar" || len(avatar.GateSites) != 2 {
		t.Errorf("mixed App Bot avatar route = %#v, want User.uploadAvatar with two gate sites", avatar)
	}
	for _, exclusion := range ManagerRouteBoundaryExclusions() {
		if route, ok := routeByKey[httpRouteKey(exclusion.Method, exclusion.Path)]; !ok || route.Handler != exclusion.Handler {
			t.Errorf("boundary exclusion has no exact source route: %#v", exclusion)
		}
	}
}

func assertGlobalOperationBoundary(t *testing.T, manifest *Manifest) {
	t.Helper()
	operationByID := make(map[string]Operation, len(manifest.Operations))
	platformAppBotCount := 0
	for _, operation := range manifest.Operations {
		operationByID[operation.ID] = operation
		if strings.HasPrefix(operation.Path, "/v1/space/") {
			t.Errorf("pure Space ACL route entered global contract: %s", operation.Path)
		}
		if strings.HasPrefix(operation.Path, "/v1/robot/") {
			t.Errorf("pure Robot business route entered global contract: %s", operation.Path)
		}
		if strings.HasPrefix(operation.Path, "/v1/groups/") &&
			operation.ID != "group.member.remove" && operation.ID != "group.member.remove_legacy" {
			t.Errorf("pure Group ACL route entered global contract: %s", operation.Path)
		}
		if strings.HasPrefix(operation.Path, "/v1/admin/app_bot") {
			platformAppBotCount++
		}
	}
	if platformAppBotCount != 9 {
		t.Errorf("platform App Bot operation count = %d, want 9", platformAppBotCount)
	}
	for _, id := range []string{"group.member.remove", "group.member.remove_legacy", "app_bot.avatar.update"} {
		operation, ok := operationByID[id]
		if !ok {
			t.Errorf("missing mixed global operation %q", id)
			continue
		}
		if operation.Scope != ScopeGlobalAdminWithBusinessACL || operation.BusinessACL == nil {
			t.Errorf("operation %q must declare business ACL fallback metadata", id)
		}
	}
	if operationByID["app_bot.token.reveal"].Scope != ScopeGlobalAdmin {
		t.Errorf("platform token reveal must not inherit Space ACL scope")
	}
}

func assertSensitiveTaxonomy(t *testing.T, manifest *Manifest) {
	t.Helper()
	operationByID := make(map[string]Operation, len(manifest.Operations))
	for _, operation := range manifest.Operations {
		operationByID[operation.ID] = operation
	}
	want := map[string]string{
		"user.password.reset":          "user.password.reset",
		"user.admin.create":            "user.admin.manage",
		"backup.download":              "backup.download",
		"message.direct_history.list":  "message.direct_history.read",
		"app_bot.token.reveal":         "app_bot.token.reveal",
		"common.system_setting.update": "system_setting.write",
		"space.destroy":                "space.destroy",
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
	}
	for _, pair := range pairs {
		if operationByID[pair[0]].Permission == operationByID[pair[1]].Permission {
			t.Errorf("risk-distinct operations %q and %q share permission %q", pair[0], pair[1], operationByID[pair[0]].Permission)
		}
	}
}

func assertProductionDoesNotConsumeRegistry(t *testing.T, root string) {
	t.Helper()
	wantImport := "github.com/Mininglamp-OSS/octo-server/pkg/authz"
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join(root, "modules"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			if strings.Trim(imported.Path.Value, "\"") == wantImport {
				t.Errorf("production handler imports generated permission registry: %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan production imports: %v", err)
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
