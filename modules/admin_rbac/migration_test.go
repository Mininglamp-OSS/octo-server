package adminrbac

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMigrationContainsOnlyTheThreeRBACTables(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "sql", "20260821000001_admin_rbac_init.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(data)
	for _, table := range []string{"admin_rbac_role", "admin_rbac_user_role", "admin_rbac_role_permission"} {
		if !strings.Contains(sql, "CREATE TABLE `"+table+"`") {
			t.Errorf("migration does not create %s", table)
		}
	}
	if strings.Contains(sql, "admin_rbac_user_revision") || strings.Contains(sql, "audit") ||
		strings.Contains(sql, "group_no") || strings.Contains(sql, "space_id") || strings.Contains(sql, "robot_id") {
		t.Fatal("migration contains an out-of-scope revision, audit or business ACL field")
	}
	if got := strings.Count(sql, "CREATE TABLE"); got != 3 {
		t.Fatalf("CREATE TABLE count = %d, want 3", got)
	}
}
