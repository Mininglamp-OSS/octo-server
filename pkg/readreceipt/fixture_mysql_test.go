package readreceipt

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/gocraft/dbr/v2"
)

// An explicit DSN must not silently reuse an old simplified fixture.
func validateReceiptFixture(t *testing.T, conn *dbr.Connection) {
	t.Helper()
	var engine, collation, databaseCollation string
	if err := conn.QueryRow(`SELECT t.ENGINE, t.TABLE_COLLATION, s.DEFAULT_COLLATION_NAME FROM information_schema.tables t JOIN information_schema.schemata s ON s.SCHEMA_NAME=t.TABLE_SCHEMA WHERE t.TABLE_SCHEMA=DATABASE() AND t.TABLE_NAME='member_readed'`).Scan(&engine, &collation, &databaseCollation); err != nil {
		t.Fatal(err)
	}
	if engine != "InnoDB" || collation != databaseCollation {
		t.Fatalf("fixture requires InnoDB and database default collation: engine=%s collation=%s default=%s", engine, collation, databaseCollation)
	}
	type column struct {
		dataType string
		width    int64
	}
	want := map[string]column{
		"id": {"bigint", 0}, "clone_no": {"varchar", 40},
		"message_id": {"varchar", 20}, "channel_id": {"varchar", 100},
		"channel_type": {"smallint", 0}, "uid": {"varchar", 40},
		"created_at": {"timestamp", 0}, "updated_at": {"timestamp", 0},
	}
	rows, err := conn.Query(`SELECT COLUMN_NAME, DATA_TYPE, CHARACTER_MAXIMUM_LENGTH, IS_NULLABLE, COLUMN_DEFAULT, EXTRA, COLUMN_TYPE, COLLATION_NAME FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='member_readed'`)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]column)
	for rows.Next() {
		var name, kind, nullable, extra, columnType string
		var width sql.NullInt64
		var defaultValue, columnCollation sql.NullString
		if err := rows.Scan(&name, &kind, &width, &nullable, &defaultValue, &extra, &columnType, &columnCollation); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		got[name] = column{kind, width.Int64}
		if nullable != "NO" || strings.Contains(columnType, "unsigned") {
			t.Errorf("fixture column %s differs from migration: %s %s", name, nullable, columnType)
		}
		switch kind {
		case "varchar":
			if columnCollation.String != databaseCollation {
				t.Errorf("fixture %s collation differs: %s", name, columnCollation.String)
			}
			if !defaultValue.Valid || defaultValue.String != "" {
				t.Errorf("fixture %s default differs: %v", name, defaultValue)
			}
		case "smallint":
			if defaultValue.String != "0" {
				t.Errorf("fixture %s default differs: %v", name, defaultValue)
			}
		case "timestamp":
			if !strings.HasPrefix(strings.ToUpper(defaultValue.String), "CURRENT_TIMESTAMP") {
				t.Errorf("fixture %s default differs: %v", name, defaultValue)
			}
		case "bigint":
			if !strings.Contains(extra, "auto_increment") {
				t.Errorf("fixture %s is not auto_increment", name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture columns differ from migration: got=%v want=%v", got, want)
	}
	indexes, err := conn.Query(`SELECT INDEX_NAME, NON_UNIQUE, COLUMN_NAME, SUB_PART FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='member_readed' ORDER BY INDEX_NAME, SEQ_IN_INDEX`)
	if err != nil {
		t.Fatal(err)
	}
	gotIndexes := make(map[string]string)
	for indexes.Next() {
		var name, column string
		var nonUnique int
		var prefix sql.NullInt64
		if err := indexes.Scan(&name, &nonUnique, &column, &prefix); err != nil {
			indexes.Close()
			t.Fatal(err)
		}
		if prefix.Valid {
			t.Errorf("fixture index %s column %s uses prefix %d", name, column, prefix.Int64)
		}
		if gotIndexes[name] == "" {
			if nonUnique == 0 {
				gotIndexes[name] = "unique:"
			} else {
				gotIndexes[name] = "index:"
			}
		} else {
			gotIndexes[name] += ","
		}
		gotIndexes[name] += column
	}
	if err := indexes.Err(); err != nil {
		indexes.Close()
		t.Fatal(err)
	}
	indexes.Close()
	wantIndexes := map[string]string{"PRIMARY": "unique:id", "message_uid_idx": "unique:message_id,uid", "channel_idx": "index:channel_id,channel_type", "uid_idx": "index:uid"}
	if !reflect.DeepEqual(gotIndexes, wantIndexes) {
		t.Fatalf("fixture indexes differ from migration: got=%v want=%v", gotIndexes, wantIndexes)
	}
}
