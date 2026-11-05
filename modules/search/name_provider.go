package search

import (
	"context"
	"database/sql"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/namesearch"
	"github.com/gocraft/dbr/v2"
)

// nameSQLProvider reads all sources in one repeatable-read snapshot. It does not
// call friend/sync, group/my or app_bot/available: those have different contracts
// and source-level limits, and cannot establish the complete S14 candidate set.
type nameSQLProvider struct{ session *dbr.Session }

func (p nameSQLProvider) Load(ctx context.Context, uid, kind string, q nameQuery) (nameCandidateSet, error) {

	switch q.Scope {
	case "", "directory", "member", "mention", "forward", "thread_filter", "summary_member", "summary_source":
	default:
		return nameCandidateSet{}, errNameScopeUnsupported
	}
	if q.Scope == "summary_member" && q.space() == "" {
		return nameCandidateSet{}, errNameForbidden
	}
	// Thread-context member selectors inherit their actual parent's authority.
	if q.Scope == "summary_member" && q.ChannelID != "" && q.ChannelType == 5 {
		parent, short, err := thread.ParseChannelID(q.ChannelID)
		if err != nil || parent == "" || short == "" {
			return nameCandidateSet{}, errNameForbidden
		}
		// Keep channel intact until the thread's existence is checked below.
	}
	tx, err := p.session.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nameCandidateSet{}, err
	}
	defer tx.RollbackUnlessCommitted()
	if q.space() != "" {
		var count int
		_, err = nameSelect(tx, "SELECT COUNT(*) FROM space_member sm INNER JOIN space s ON s.space_id=sm.space_id WHERE sm.uid=? AND sm.space_id=? AND sm.status=1 AND s.status=1", uid, q.space()).LoadContext(ctx, &count)
		if err != nil {
			return nameCandidateSet{}, err
		}
		if count == 0 {
			return nameCandidateSet{}, errNameForbidden
		}
	}
	if q.Scope == "member" || q.Scope == "mention" || q.Scope == "thread_filter" || (q.Scope == "summary_member" && q.ChannelID != "") {
		if q.ChannelID == "" {
			return nameCandidateSet{}, errNameScopeUnsupported
		}

		if q.ChannelType == 5 {
			parent, short, _ := thread.ParseChannelID(q.ChannelID)
			var found int
			_, err = nameSelect(tx, "SELECT COUNT(*) FROM thread WHERE group_no=? AND short_id=? AND status IN (1,2)", parent, short).LoadContext(ctx, &found)
			if err != nil {
				return nameCandidateSet{}, err
			}
			if found == 0 {
				return nameCandidateSet{}, errNameForbidden
			}
			q.ChannelID, q.ChannelType = parent, 2
		}
		var count int
		_, err = nameSelect(tx, "SELECT COUNT(*) FROM group_member gm INNER JOIN `group` g ON g.group_no=gm.group_no WHERE gm.group_no=? AND gm.uid=? AND gm.is_deleted=0 AND gm.status=1 AND g.status=1 AND g.purpose='' AND (g.space_id='' OR EXISTS(SELECT 1 FROM space s WHERE s.space_id=g.space_id AND s.status=1))", q.ChannelID, uid).LoadContext(ctx, &count)
		if err != nil {
			return nameCandidateSet{}, err
		}
		if count == 0 {
			return nameCandidateSet{}, errNameForbidden
		}
	}
	var set nameCandidateSet
	if kind == "contact" {
		set, err = loadNameContacts(ctx, tx, uid, q)
	} else if kind == "group" {
		set, err = loadNameGroups(ctx, tx, uid, q)
	} else if kind == "thread" {
		set, err = loadNameThreads(ctx, tx, uid, q)
	} else {
		return nameCandidateSet{}, errNameScopeUnsupported
	}
	if err != nil {
		return nameCandidateSet{}, err
	}
	if err = tx.Commit(); err != nil {
		return nameCandidateSet{}, err
	}
	return set, nil
}

func loadNameContacts(ctx context.Context, tx *dbr.Tx, uid string, q nameQuery) (nameCandidateSet, error) {
	// app_bot is joined even for unpublished apps, to prevent another candidate
	// source from treating an unpublished/foreign app as an ordinary Space Bot.
	query := `SELECT u.uid, IFNULL(u.name,'') name, IFNULL(uv.real_name,'') real_name,
 IFNULL(us.remark,'') remark, u.robot,
 EXISTS(SELECT 1 FROM space_member sm WHERE sm.uid=u.uid AND sm.space_id=? AND sm.status=1) is_member,
 (f.to_uid IS NOT NULL) friend, (r.robot_id IS NOT NULL AND r.status=1) ordinary_active,
 IFNULL(ab.id,'') app_id, IFNULL(ab.display_name,'') app_name, IFNULL(ab.scope,'') app_scope,
 IFNULL(ab.space_id,'') app_space, IFNULL(ab.status,0) app_status
 FROM ` + "`user`" + ` u
 LEFT JOIN user_verification uv ON uv.user_id=u.uid
 LEFT JOIN friend f ON f.to_uid=u.uid AND f.uid=? AND f.is_deleted=0
 LEFT JOIN user_setting us ON us.to_uid=u.uid AND us.uid=?
 LEFT JOIN app_bot ab ON ab.uid=u.uid
 LEFT JOIN robot r ON r.robot_id=u.uid
 WHERE u.status=1 AND u.is_destroy<>2 AND u.uid<>?
 AND (f.to_uid IS NOT NULL
 OR (?<>'' AND EXISTS(SELECT 1 FROM space_member sm WHERE sm.uid=u.uid AND sm.space_id=? AND sm.status=1))
 OR (ab.status=1 AND (ab.scope='platform' OR (ab.scope='space' AND ab.space_id=? AND ?<>''))))`
	args := []interface{}{q.space(), uid, uid, uid, q.space(), q.space(), q.space(), q.space()}
	if (q.Scope == "summary_member" && q.ChannelID != "") || q.Scope == "summary_source" || q.Scope == "member" || q.Scope == "mention" {
		// This operation owns an authorized group roster, including external humans.
		// R6 adds operation eligibility; do not lose members outside directory R1.
		query = query[:strings.Index(query, "\n AND (f.to_uid IS NOT NULL")]
		args = args[:4]
	}

	if q.Scope == "member" || q.Scope == "mention" || (q.Scope == "summary_member" && q.ChannelID != "") {
		query += " AND EXISTS(SELECT 1 FROM group_member gm WHERE gm.uid=u.uid AND gm.group_no=? AND gm.is_deleted=0 AND gm.status=1)"
		args = append(args, q.ChannelID)
	}

	if q.Scope == "summary_member" {
		query += " AND u.robot=0 AND ab.id IS NULL AND r.robot_id IS NULL "
		if q.ChannelID == "" {
			query += " AND LENGTH(u.uid)=32 AND EXISTS(SELECT 1 FROM space_member sm WHERE sm.uid=u.uid AND sm.space_id=? AND sm.status=1)"
			args = append(args, q.space())
		}
	}
	if q.Scope == "summary_source" {
		// Preserve the summary service's DM-source eligibility, not all R1 contacts.
		query += ` AND EXISTS(SELECT 1 FROM conversation_extra ce WHERE ce.uid=? AND ce.channel_type=1 AND ce.channel_id=u.uid)
 AND u.uid NOT IN ('fileHelper','botfather') AND
 ((LENGTH(u.uid)=32 AND u.robot=0 AND (?='' OR EXISTS(SELECT 1 FROM space_member sm WHERE sm.uid=u.uid AND sm.space_id=? AND sm.status=1)))
 OR (f.to_uid IS NOT NULL AND r.robot_id IS NOT NULL AND r.creator_uid<>'' AND r.status=1 AND EXISTS(SELECT 1 FROM space_member sm WHERE sm.uid=u.uid AND sm.status=1 AND (?='' OR sm.space_id=?))))`
		args = append(args, uid, q.space(), q.space(), q.space(), q.space())
	}
	var rows []nameContactRow
	if _, err := nameSelect(tx, query, args...).LoadContext(ctx, &rows); err != nil {
		return nameCandidateSet{}, err
	}

	authorizedRoster := (q.Scope == "summary_member" && q.ChannelID != "") || q.Scope == "summary_source" || q.Scope == "member" || q.Scope == "mention"
	return eligibleNameContactsInScope(rows, uid, q.space(), authorizedRoster), nil
}

func loadNameGroups(ctx context.Context, tx *dbr.Tx, uid string, q nameQuery) (nameCandidateSet, error) {
	var rows []struct {
		ID     string `db:"id"`
		Name   string `db:"original_name"`
		Remark string `db:"remark_name"`
	}
	query := `SELECT g.group_no id, IFNULL(g.name,'') original_name, IFNULL(gs.remark,'') remark_name
 FROM ` + "`group`" + ` g INNER JOIN group_member gm ON gm.group_no=g.group_no
 LEFT JOIN group_setting gs ON gs.group_no=g.group_no AND gs.uid=?
 WHERE gm.uid=? AND gm.is_deleted=0 AND gm.status=1 AND g.status=1 AND g.purpose=''
 AND (g.space_id='' OR EXISTS(SELECT 1 FROM space s WHERE s.space_id=g.space_id AND s.status=1))`
	args := []interface{}{uid, uid}
	if q.Scope == "summary_source" && q.space() != "" {
		query += " AND g.space_id=?"
		args = append(args, q.space())
	}
	_, err := nameSelect(tx, query, args...).LoadContext(ctx, &rows)
	if err != nil {
		return nameCandidateSet{}, err
	}
	set := nameCandidateSet{Complete: true, Candidates: []namesearch.Candidate{}}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		set.Candidates = append(set.Candidates, namesearch.Candidate{ID: r.ID, Kind: "group", OriginalName: r.Name, RemarkName: r.Remark})
	}
	return set, nil
}
