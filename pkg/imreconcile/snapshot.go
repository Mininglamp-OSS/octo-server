package imreconcile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

type memberPage struct {
	Start       string
	End         string
	Subscribers []string
	ParentDeny  []string
	ChildDeny   []string
}

type channelWork struct {
	ChildID int64
	Deleted bool
	Snapshot
}

// Captured while the authority row is locked: every business writer must lock
// that row before its member/child mutation can commit. All reads share one
// repeatable-read view. A subsequent revision cannot change these stored pages.
// HTTP never runs in this transaction. Only one snapshot per group is retained.
func captureSnapshot(ctx context.Context, tx *sql.Tx, group string, revision uint64) error {
	var status, groupType int
	err := tx.QueryRowContext(ctx, "SELECT status,group_type FROM `group` WHERE group_no=?", group).Scan(&status, &groupType)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	exists := err == nil
	var subscribers, parentDeny, childDeny []string
	if exists {
		rows, err := tx.QueryContext(ctx, `SELECT uid,status,forbidden_expir_time FROM group_member
			WHERE group_no=? AND is_deleted=0 ORDER BY uid`, group)
		if err != nil {
			return err
		}
		for rows.Next() {
			var uid string
			var memberStatus, forbidden int
			if err = rows.Scan(&uid, &memberStatus, &forbidden); err != nil {
				break
			}
			if memberStatus == int(common.GroupMemberStatusNormal) {
				subscribers = append(subscribers, uid)
			}
			if memberStatus == int(common.GroupMemberStatusBlacklist) {
				parentDeny = append(parentDeny, uid)
				childDeny = append(childDeny, uid)
			} else if forbidden != 0 {
				parentDeny = append(parentDeny, uid)
			}
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return err
		}
	}
	pages := splitMemberPages(subscribers, parentDeny, childDeny)
	for i, page := range pages {
		payload, err := json.Marshal(page)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO im_reconcile_snapshot_page
			(group_no,revision,page_index,payload) VALUES (?,?,?,?)`, group, revision, i, payload); err != nil {
			return err
		}
	}
	makeChannel := func(id int64, channel string, channelType uint8, deleted, banned bool) channelWork {
		s := Snapshot{ChannelID: channel, ChannelType: channelType, Revision: revision,
			OperationID: fmt.Sprintf("business-%d", revision), PageCount: uint32(len(pages))}
		if groupType == 1 {
			s.Large = 1
		}
		// Preserve legacy semantics: disabling a parent does not ban every child.
		// A soft-deleted child has its own ban, while history remains readable.
		if banned || (channelType == 2 && exists && status == 0) {
			s.Ban = 1
		}
		if deleted || (exists && status == 2) {
			s.Disband = 1
		}
		if !deleted {
			s.Subscribers = subscribers
			s.Denylist = childDeny
			if channelType == 2 {
				s.Denylist = parentDeny
			}
		} else {
			s.PageCount = 1
		}
		// Identity covers the complete immutable payload, before slicing a page.
		encoded, _ := json.Marshal(s) // Snapshot contains only scalar/string fields.
		digest := sha256.Sum256(encoded)
		s.SnapshotID = hex.EncodeToString(digest[:])
		s.Subscribers, s.Denylist = nil, nil
		return channelWork{ChildID: id, Deleted: deleted, Snapshot: s}
	}
	channels := map[int64]channelWork{0: makeChannel(0, group, 2, !exists, false)}
	rows, err := tx.QueryContext(ctx, "SELECT id,short_id,status FROM thread WHERE group_no=? ORDER BY id", group)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var shortID string
		var state int
		if err = rows.Scan(&id, &shortID, &state); err != nil {
			break
		}
		channels[id] = makeChannel(id, group+"____"+shortID, 5, !exists, state == 3)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT thread_id,channel_id FROM im_reconcile_deleted_channel
		WHERE group_no=? AND completed_revision=0 ORDER BY thread_id`, group)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var channel string
		if err = rows.Scan(&id, &channel); err != nil {
			break
		}
		channels[id] = makeChannel(id, channel, 5, true, false)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	ids := make([]int64, 0, len(channels))
	for id := range channels {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		channel := channels[id]
		payload, err := json.Marshal(channel.Snapshot)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO im_reconcile_snapshot_channel
			(group_no,revision,child_id,page_count,deleted,payload) VALUES (?,?,?,?,?,?)`,
			group, revision, id, channel.PageCount, channel.Deleted, payload); err != nil {
			return err
		}
	}
	return nil
}

func splitMemberPages(subscribers, parentDeny, childDeny []string) []memberPage {
	// Go's byte ordering matches the receiver's range semantics, unlike SQL
	// case-insensitive collation. Never infer page ranges from SQL row positions.
	for _, list := range [][]string{subscribers, parentDeny, childDeny} {
		sort.Strings(list)
	}
	union := append(append(append([]string(nil), subscribers...), parentDeny...), childDeny...)
	sort.Strings(union)
	unique := union[:0]
	for _, uid := range union {
		if len(unique) == 0 || unique[len(unique)-1] != uid {
			unique = append(unique, uid)
		}
	}
	pages := make([]memberPage, max(1, (len(unique)+memberPageSize-1)/memberPageSize))
	for i := range pages {
		page := &pages[i]
		start, end := i*memberPageSize, min((i+1)*memberPageSize, len(unique))
		if start > 0 {
			page.Start = unique[start]
		}
		if end < len(unique) {
			page.End = unique[end]
		}
		page.Subscribers = membersInRange(subscribers, page.Start, page.End)
		page.ParentDeny = membersInRange(parentDeny, page.Start, page.End)
		page.ChildDeny = membersInRange(childDeny, page.Start, page.End)
	}
	return pages
}

func membersInRange(members []string, start, end string) []string {
	first := sort.SearchStrings(members, start)
	last := len(members)
	if end != "" {
		last = sort.SearchStrings(members, end)
	}
	return members[first:last]
}

func loadSnapshotWork(ctx context.Context, tx *sql.Tx, group string, revision uint64) ([]channelWork, error) {
	// Four concurrent requests fit one scheduling quantum. A failed sibling
	// keeps only its own cursor; successful pages will not be sent again.
	rows, err := tx.QueryContext(ctx, `SELECT child_id,member_page,deleted,payload
		FROM im_reconcile_snapshot_channel WHERE group_no=? AND revision=? AND complete=0
		ORDER BY child_id LIMIT 4`, group, revision)
	if err != nil {
		return nil, err
	}
	var work []channelWork
	for rows.Next() {
		var c channelWork
		var index uint32
		var payload []byte
		if err = rows.Scan(&c.ChildID, &index, &c.Deleted, &payload); err != nil {
			break
		}
		if err = json.Unmarshal(payload, &c.Snapshot); err != nil {
			break
		}
		if c.Revision != revision || index >= c.PageCount {
			err = errors.New("invalid stored IM snapshot channel")
			break
		}
		c.PageIndex = index
		// Receiver receipt identity is (channel_id, channel_type, operation_id),
		// and its digest rejects changed bodies. The same ID on siblings is safe;
		// the header must equal the body's operation_id, not replace that tuple.
		c.OperationID = fmt.Sprintf("business-%d-%d", revision, index)
		work = append(work, c)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	pages := make(map[uint32]memberPage)
	for i := range work {
		c := &work[i]
		if c.Deleted {
			continue
		}
		page, ok := pages[c.PageIndex]
		if !ok {
			var payload []byte
			if err := tx.QueryRowContext(ctx, `SELECT payload FROM im_reconcile_snapshot_page
				WHERE group_no=? AND revision=? AND page_index=?`, group, revision, c.PageIndex).Scan(&payload); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(payload, &page); err != nil {
				return nil, err
			}
			pages[c.PageIndex] = page
		}
		c.RangeStart, c.RangeEnd, c.Subscribers = page.Start, page.End, page.Subscribers
		c.Denylist = page.ChildDeny
		if c.ChannelType == 2 {
			c.Denylist = page.ParentDeny
		}
	}
	if len(work) == 0 {
		return nil, errors.New("active IM snapshot has no unfinished channels")
	}
	return work, nil
}

func deleteSnapshot(ctx context.Context, tx *sql.Tx, group string) error {
	for _, table := range []string{"im_reconcile_snapshot_page", "im_reconcile_snapshot_channel"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE group_no=?", group); err != nil {
			return err
		}
	}
	return nil
}
