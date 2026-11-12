package readreceipt

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	messagesql "github.com/Mininglamp-OSS/octo-server/modules/message/sql"
)

const fixtureMigration = "20210407000001_message_legacy01.sql"

// Pin the migration filename and all four statements. Moving/splitting the
// migration must fail loudly rather than silently constructing a partial table.
func receiptFixtureStatements(source fs.FS) ([]string, error) {
	data, err := fs.ReadFile(source, fixtureMigration)
	if err != nil {
		return nil, err
	}
	var clean []string
	for _, line := range strings.Split(string(data), "\n") {
		clean = append(clean, strings.SplitN(line, "--", 2)[0])
	}
	index := regexp.MustCompile("(?i)^CREATE\\s+(?:UNIQUE\\s+)?INDEX\\s+(\\w+)\\s+on\\s+`member_readed`\\s*\\(")
	want := map[string]bool{"table": false, "channel_idx": false, "uid_idx": false, "message_uid_idx": false}
	var result []string
	for _, part := range strings.Split(strings.Join(clean, "\n"), ";") {
		statement := strings.TrimSpace(part)
		key := ""
		if strings.HasPrefix(statement, "CREATE TABLE `member_readed`(") {
			key = "table"
		}
		if match := index.FindStringSubmatch(statement); match != nil {
			key = match[1]
		}
		if key == "" {
			continue
		}
		seen, ok := want[key]
		if !ok || seen {
			return nil, fmt.Errorf("unexpected/duplicate fixture statement %s", key)
		}
		want[key] = true
		result = append(result, statement)
	}
	for key, seen := range want {
		if !seen {
			return nil, fmt.Errorf("migration missing fixture statement %s", key)
		}
	}
	return result, nil
}

func TestReceiptFixtureEmbeddedSource(t *testing.T) {
	statements, err := receiptFixtureStatements(messagesql.Files)
	if err != nil || len(statements) != 4 {
		t.Fatalf("embedded migration: %v %v", statements, err)
	}
	data, err := messagesql.Files.ReadFile(fixtureMigration)
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]fs.FS{
		"renamed": fstest.MapFS{"renamed.sql": &fstest.MapFile{Data: data}},
		"split":   fstest.MapFS{fixtureMigration: &fstest.MapFile{Data: []byte(strings.ReplaceAll(string(data), "CREATE UNIQUE INDEX message_uid_idx", "CREATE UNIQUE INDEX moved_idx"))}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := receiptFixtureStatements(source); err == nil {
				t.Fatal("changed migration must fail loudly")
			}
		})
	}
}
