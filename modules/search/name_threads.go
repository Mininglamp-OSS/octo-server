package search

import (
	"context"
	"github.com/Mininglamp-OSS/octo-server/pkg/namesearch"
	"github.com/gocraft/dbr/v2"
)

// Threads inherit the active parent-group ACL, not thread_member, recent caches
// or a caller-provided allowed list. They never enter the default group directory.
func loadNameThreads(ctx context.Context, tx *dbr.Tx, uid string, q nameQuery) (nameCandidateSet, error) {
	if q.Scope != "forward" && q.Scope != "thread_filter" && q.Scope != "summary_source" {
		return nameCandidateSet{}, errNameScopeUnsupported
	}
	query := `SELECT CONCAT(t.group_no,'____',t.short_id) id,t.name original_name,t.group_no parent_id,(t.status=2) archived
 FROM thread t INNER JOIN ` + "`group`" + ` g ON g.group_no=t.group_no
 INNER JOIN group_member gm ON gm.group_no=g.group_no
 WHERE gm.uid=? AND gm.is_deleted=0 AND gm.status=1 AND g.status=1 AND g.purpose=''
 AND (g.space_id='' OR EXISTS(SELECT 1 FROM space s WHERE s.space_id=g.space_id AND s.status=1))`
	args := []interface{}{uid}
	if q.IncludeArchived && q.Scope != "forward" {
		query += " AND t.status IN (1,2)"
	} else {
		query += " AND t.status=1"
	}
	if q.Scope == "forward" {
		query += " AND (g.forbidden=0 OR gm.role IN (1,2)) AND (gm.forbidden_expir_time=0 OR gm.forbidden_expir_time<UNIX_TIMESTAMP())"
	}
	if q.Scope == "thread_filter" {
		query += " AND g.group_no=?"
		args = append(args, q.ChannelID)
	}
	if q.Scope == "summary_source" {
		query += " AND t.message_count>0"
		if q.space() != "" {
			query += " AND g.space_id=?"
			args = append(args, q.space())
		}
	}
	var rows []struct {
		ID       string `db:"id"`
		Name     string `db:"original_name"`
		Parent   string `db:"parent_id"`
		Archived bool   `db:"archived"`
	}
	if _, err := nameSelect(tx, query, args...).LoadContext(ctx, &rows); err != nil {
		return nameCandidateSet{}, err
	}
	set := nameCandidateSet{Complete: true, Candidates: []namesearch.Candidate{}}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		set.Candidates = append(set.Candidates, namesearch.Candidate{ID: r.ID, Kind: "thread", OriginalName: r.Name, ParentID: r.Parent, Archived: r.Archived})
	}
	return set, nil
}
