package bot_api

import (
	"os"
	"strings"
	"testing"
)

func TestGenericScopeDownMigrationGuardsMySQLColumnDrops(t *testing.T) {
	raw, err := os.ReadFile("sql/20260921000001_obo_generic_scope.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	if strings.Contains(sql, "DROP COLUMN IF EXISTS") {
		t.Fatal("MySQL 8 does not support DROP COLUMN IF EXISTS")
	}
	for _, required := range []string{
		"CREATE PROCEDURE __obo_generic_scope_down()",
		"COLUMN_NAME = 'expires_at'",
		"COLUMN_NAME = 'policy_version'",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("Down migration is missing %q", required)
		}
	}
}
