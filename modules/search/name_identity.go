package search

import (
	"regexp"
	"strings"

	"github.com/gocraft/dbr/v2"
)

// IDs are not human-language text. Legacy/default-collation tables coexist with
// explicitly general_ci migrations. Fold ASCII case only, then compare bytes:
// never let general_ci equate distinct IDs such as s/ß, or fold accents.
// This transformer only handles identifier comparisons in our static SQL; it
// never receives SQL supplied by a caller and never touches name matching.
var nameIdentityComparison = regexp.MustCompile(`\b((?:[a-z]+\.)?(?:uid|user_id|to_uid|robot_id|space_id|group_no|short_id|channel_id))\s*(=|<>|!=)\s*((?:[a-z]+\.)?(?:uid|user_id|to_uid|robot_id|space_id|group_no|short_id|channel_id)|\?)`)

func nameFoldID(id string) string {
	b := []byte(id)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func nameColumnIdentity(column string) string {
	// ASCII IDs take one LOWER; unrestricted legacy IDs retain exact non-ASCII
	// bytes and only fold embedded A-Z. Identical to nameFoldID, not Unicode lower.
	folded := column
	for c := 'A'; c <= 'Z'; c++ {
		folded = "REPLACE(" + folded + ",'" + string(c) + "','" + string(c+32) + "')"
	}
	return "CAST(IF(OCTET_LENGTH(" + column + ")=CHAR_LENGTH(" + column + "),LOWER(" + column + ")," + folded + ") AS BINARY)"
}

func nameIdentitySQL(query string) string {
	return nameIdentityComparison.ReplaceAllStringFunc(query, func(match string) string {
		parts := nameIdentityComparison.FindStringSubmatch(match)
		right := "CAST(? AS BINARY)"
		if parts[3] != "?" {
			right = nameColumnIdentity(parts[3])
		}
		return nameColumnIdentity(parts[1]) + " " + parts[2] + " " + right
	})
}

func nameSelect(tx *dbr.Tx, query string, args ...interface{}) *dbr.SelectStmt {
	folded := make([]interface{}, len(args))
	for i, arg := range args {
		if id, ok := arg.(string); ok {
			folded[i] = nameFoldID(id)
		} else {
			folded[i] = arg
		}
	}
	return tx.SelectBySql(nameIdentitySQL(strings.TrimSpace(query)), folded...)
}
