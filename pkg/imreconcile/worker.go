package imreconcile

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

const channelPageSize = 16
const memberPageSize = 128

// Bound work from foreground requests and background recovery together.
var groupSlots = make(chan struct{}, 4)
var snapshotHTTP = &http.Client{Timeout: 6 * time.Second}

type Snapshot struct {
	ChannelID   string   `json:"channel_id"`
	ChannelType uint8    `json:"channel_type"`
	OperationID string   `json:"operation_id"`
	Revision    uint64   `json:"revision"`
	SnapshotID  string   `json:"snapshot_id,omitempty"`
	PageIndex   uint32   `json:"page_index,omitempty"`
	PageCount   uint32   `json:"page_count,omitempty"`
	RangeStart  string   `json:"range_start,omitempty"`
	RangeEnd    string   `json:"range_end,omitempty"`
	Subscribers []string `json:"subscribers"`
	Denylist    []string `json:"denylist"`
	Ban         int      `json:"ban"`
	Large       int      `json:"large"`
	Disband     int      `json:"disband"`
}

type Sender func(context.Context, Snapshot) error

type Worker struct {
	db     *sql.DB
	send   Sender
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(ctx *config.Context) *Worker {
	cfg := ctx.GetConfig()
	return &Worker{db: ctx.DB().DB, send: func(c context.Context, snapshot Snapshot) error {
		data, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(c, http.MethodPost, strings.TrimRight(cfg.WuKongIM.APIURL, "/")+"/channel/subscriber_reconcile", bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", snapshot.OperationID)
		if cfg.WuKongIM.ManagerToken != "" {
			req.Header.Set("token", cfg.WuKongIM.ManagerToken)
		}
		resp, err := snapshotHTTP.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("IM subscriber reconciliation returned HTTP %d", resp.StatusCode)
		}
		return readErr
	}}
}

func (w *Worker) Start() error {
	if !Enabled() {
		var exists int
		err := w.db.QueryRow("SELECT 1 FROM im_group_reconcile LIMIT 1").Scan(&exists)
		if err == nil {
			return errors.New("DM_IM_RECONCILE_ENABLED cannot be disabled after authority activation")
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.adoptExisting(ctx)
	}()
	for i := 0; i < 2; i++ {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					w.runPending(ctx)
				}
			}
		}()
	}
	return nil
}

func (w *Worker) Stop() error {
	if w.cancel != nil {
		w.cancel()
		w.wg.Wait()
	}
	return nil
}

func (w *Worker) runPending(ctx context.Context) {
	rows, err := w.db.QueryContext(ctx, `SELECT group_no FROM im_group_reconcile
        WHERE pending=1 AND next_attempt_at<=UTC_TIMESTAMP(6)
        AND (lease_until IS NULL OR lease_until<=UTC_TIMESTAMP(6))
        ORDER BY next_attempt_at, group_no LIMIT 4`)
	if err != nil {
		log.Error("query IM reconciliation queue failed", zap.Error(err))
		return
	}
	var groups []string
	for rows.Next() {
		var group string
		if err = rows.Scan(&group); err != nil {
			break
		}
		groups = append(groups, group)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		log.Error("read IM reconciliation queue failed", zap.Error(err))
		return
	}
	for _, group := range groups {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := w.step(attemptCtx, group)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Error("IM reconciliation remains pending", zap.String("group_no", group), zap.Error(err))
		}
	}
}

// Flush waits for the transaction's revision (or a newer authoritative one).
// A timeout leaves the SQL intent pending; it is not converted into completion.
func Flush(ctx *config.Context, group string) error {
	if !Enabled() {
		return errors.New("IM reconciliation is not enabled")
	}
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return New(ctx).flush(c, group)
}

func (w *Worker) flush(c context.Context, group string) error {
	var lastErr error
	var wanted uint64
	if err := w.db.QueryRowContext(c, "SELECT revision FROM im_group_reconcile WHERE group_no=?", group).Scan(&wanted); err != nil {
		return fmt.Errorf("missing durable IM intent: %w", err)
	}
	for {
		var completed uint64
		if err := w.db.QueryRowContext(c, "SELECT completed_revision FROM im_group_reconcile WHERE group_no=?", group).Scan(&completed); err != nil {
			return errors.Join(err, lastErr)
		}
		if completed >= wanted {
			return nil
		}
		progress, err := w.step(c, group)
		if err != nil {
			lastErr = err
		}
		if !progress {
			select {
			case <-c.Done():
				return errors.Join(c.Err(), lastErr)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

type page struct {
	group      string
	revision   uint64
	owner      string
	lastChild  int64
	memberPage uint32
	lastPage   bool
	attempts   uint32
	snapshots  []Snapshot
}

func (w *Worker) step(ctx context.Context, group string) (bool, error) {
	select {
	case groupSlots <- struct{}{}:
		defer func() { <-groupSlots }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	p, err := w.claim(ctx, group)
	if err != nil || p == nil {
		return false, err
	}
	// The claim transaction has committed. No SQL locks are held over HTTP.
	results := make([]error, len(p.snapshots))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, snapshot := range p.snapshots {
		slots <- struct{}{}
		wg.Add(1)
		go func(i int, snapshot Snapshot) {
			defer wg.Done()
			defer func() { <-slots }()
			results[i] = w.send(ctx, snapshot)
		}(i, snapshot)
	}
	wg.Wait()
	err = errors.Join(results...)
	// Persist outcome even if the original HTTP client has disconnected.
	finishCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err != nil {
		delay := time.Second * time.Duration(1<<min(p.attempts, 8))
		message := err.Error()
		if len(message) > 512 {
			message = message[:512]
		}
		_, finishErr := w.db.ExecContext(finishCtx, `UPDATE im_group_reconcile
            SET attempts=attempts+1, next_attempt_at=DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? SECOND), lease_owner='',
            lease_until=NULL, last_error=? WHERE group_no=? AND revision=? AND lease_owner=?`,
			int64(delay/time.Second), message, group, p.revision, p.owner)
		return false, errors.Join(err, finishErr)
	}
	query := `UPDATE im_group_reconcile SET child_cursor=?, member_page=?, lease_owner='',
        lease_until=NULL, last_error='', attempts=0, next_attempt_at=UTC_TIMESTAMP(6)
        WHERE group_no=? AND revision=? AND lease_owner=?`
	if p.lastPage {
		query = `UPDATE im_group_reconcile SET child_cursor=?, member_page=?, completed_revision=revision,
            pending=0, lease_owner='', lease_until=NULL, last_error='', attempts=0
            WHERE group_no=? AND revision=? AND lease_owner=?`
	}
	result, err := w.db.ExecContext(finishCtx, query, p.lastChild, p.memberPage, group, p.revision, p.owner)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err // superseded workers cannot clear newer work
}

func (w *Worker) claim(ctx context.Context, group string) (*page, error) {
	tx, err := w.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p := &page{group: group}
	var claimable bool
	err = tx.QueryRowContext(ctx, `SELECT group_no, revision, child_cursor, member_page, attempts,
        pending=1 AND next_attempt_at<=UTC_TIMESTAMP(6)
        AND (lease_until IS NULL OR lease_until<=UTC_TIMESTAMP(6))
        FROM im_group_reconcile WHERE group_no=? FOR UPDATE`, group).
		Scan(&p.group, &p.revision, &p.lastChild, &p.memberPage, &p.attempts, &claimable)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !claimable {
		return nil, nil
	}
	group = p.group
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	p.owner = hex.EncodeToString(token[:])
	if _, err = tx.ExecContext(ctx, `UPDATE im_group_reconcile SET lease_owner=?,
        lease_until=DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 30 SECOND) WHERE group_no=?`, p.owner, group); err != nil {
		return nil, err
	}
	// The dirty row blocks concurrent mutations from committing their matching
	// intent. The following consistent reads therefore belong to this revision.
	var status, groupType int
	err = tx.QueryRowContext(ctx, "SELECT status, group_type FROM `group` WHERE group_no=?", group).Scan(&status, &groupType)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	exists := err == nil // Disband preserves historical memberships/conversations.
	var subscribers, parentDeny, childDeny []string
	if exists {
		rows, err := tx.QueryContext(ctx, `SELECT uid, status, forbidden_expir_time FROM group_member
            WHERE group_no=? AND is_deleted=0 ORDER BY uid`, group)
		if err != nil {
			return nil, err
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
				// Existing per-member mutes are installed on the parent channel.
				parentDeny = append(parentDeny, uid)
			}
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	makeSnapshot := func(channel string, channelType uint8, deleted bool) Snapshot {
		s := Snapshot{ChannelID: channel, ChannelType: channelType, Revision: p.revision,
			OperationID: fmt.Sprintf("business-%d", p.revision)}
		if groupType == 1 {
			s.Large = 1
		}
		if exists && status == 0 {
			s.Ban = 1
		}
		if deleted || (exists && status == 2) {
			s.Disband = 1
		}
		if !deleted {
			s.Subscribers = subscribers
			if channelType == 2 {
				s.Denylist = parentDeny
			} else {
				s.Denylist = childDeny
			}
		}
		return s
	}
	if p.lastChild == 0 {
		p.snapshots = append(p.snapshots, makeSnapshot(group, 2, !exists))
	}
	type child struct {
		id      int64
		channel string
		deleted bool
		banned  bool
	}
	children := make(map[int64]child)
	rows, err := tx.QueryContext(ctx, `SELECT id, short_id, status FROM thread
        WHERE group_no=? AND id>? ORDER BY id LIMIT ?`, group, p.lastChild, channelPageSize+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var shortID string
		var state int
		if err = rows.Scan(&id, &shortID, &state); err != nil {
			break
		}
		children[id] = child{id: id, channel: group + "____" + shortID, deleted: !exists}
		if state == 3 {
			c := children[id]
			c.banned = true
			children[id] = c
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT thread_id, channel_id FROM im_reconcile_deleted_channel
        WHERE group_no=? AND thread_id>? ORDER BY thread_id LIMIT ?`, group, p.lastChild, channelPageSize+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var channel string
		if err = rows.Scan(&id, &channel); err != nil {
			break
		}
		children[id] = child{id: id, channel: channel, deleted: true}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	ordered := make([]child, 0, len(children))
	for _, c := range children {
		ordered = append(ordered, c)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].id < ordered[j].id })
	p.lastPage = len(ordered) <= channelPageSize
	if len(ordered) > channelPageSize {
		ordered = ordered[:channelPageSize]
	}
	originalChildCursor := p.lastChild
	for _, child := range ordered {
		snapshot := makeSnapshot(child.channel, 5, child.deleted)
		if child.banned {
			snapshot.Ban = 1
		}
		p.snapshots = append(p.snapshots, snapshot)
		p.lastChild = child.id
	}
	// Page the authority by lexical UID ranges. The last range includes every
	// old UID beyond the desired set, so removals are complete too. Keep a
	// durable member cursor in addition to the child cursor: a large group
	// cannot starve by replaying its first pages on each five-second attempt.
	union := append(append(append([]string(nil), subscribers...), parentDeny...), childDeny...)
	sort.Strings(union)
	unique := union[:0]
	for _, uid := range union {
		if len(unique) == 0 || unique[len(unique)-1] != uid {
			unique = append(unique, uid)
		}
	}
	pageCount := uint32(max(1, (len(unique)+memberPageSize-1)/memberPageSize))
	if p.memberPage >= pageCount {
		return nil, errors.New("IM member cursor exceeds authoritative revision")
	}
	start := int(p.memberPage) * memberPageSize
	end := min(start+memberPageSize, len(unique))
	rangeStart, rangeEnd := "", ""
	if start > 0 {
		rangeStart = unique[start]
	}
	if end < len(unique) {
		rangeEnd = unique[end]
	}
	inRange := func(uids []string) []string {
		result := []string(nil)
		for _, uid := range uids {
			if uid >= rangeStart && (rangeEnd == "" || uid < rangeEnd) {
				result = append(result, uid)
			}
		}
		sort.Strings(result)
		return result
	}
	for i := range p.snapshots {
		snapshot := &p.snapshots[i]
		sort.Strings(snapshot.Subscribers)
		sort.Strings(snapshot.Denylist)
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(encoded)
		snapshot.SnapshotID = hex.EncodeToString(digest[:])
		snapshot.PageIndex, snapshot.PageCount = p.memberPage, pageCount
		snapshot.RangeStart, snapshot.RangeEnd = rangeStart, rangeEnd
		snapshot.OperationID = fmt.Sprintf("business-%d-%d", p.revision, p.memberPage)
		snapshot.Subscribers, snapshot.Denylist = inRange(snapshot.Subscribers), inRange(snapshot.Denylist)
	}
	if p.memberPage+1 < pageCount {
		p.memberPage++
		p.lastChild = originalChildCursor
		p.lastPage = false
	} else {
		p.memberPage = 0
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

// Adoption is a bounded primary-key scan, separate from normal mutation
// capture. Existing authority is never reset, and process restart may safely
// repeat the scan. There is no failure-only enqueue fallback in Flush.
func (w *Worker) adoptExisting(ctx context.Context) {
	cursor := ""
	for ctx.Err() == nil {
		next, done, err := w.adoptPage(ctx, cursor)
		if err == nil {
			cursor = next
			if done {
				return
			}
		} else {
			log.Error("adopt existing IM groups failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (w *Worker) adoptPage(ctx context.Context, cursor string) (string, bool, error) {
	rows, err := w.db.QueryContext(ctx, "SELECT group_no FROM `group` WHERE group_no>? ORDER BY group_no LIMIT 64", cursor)
	if err != nil {
		return cursor, false, err
	}
	groups := []string{}
	for rows.Next() {
		var group string
		if err = rows.Scan(&group); err != nil {
			break
		}
		groups = append(groups, group)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return cursor, false, err
	}
	for _, group := range groups {
		if err := func() error {
			tx, err := w.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var locked string
			err = tx.QueryRowContext(ctx, "SELECT group_no FROM `group` WHERE group_no=? FOR UPDATE", group).Scan(&locked)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO im_group_reconcile (group_no,revision,pending,next_attempt_at)
                VALUES (?,1,1,UTC_TIMESTAMP(6)) ON DUPLICATE KEY UPDATE group_no=group_no`, group)
			if err != nil {
				return err
			}
			return tx.Commit()
		}(); err != nil {
			return cursor, false, err
		}
		cursor = group
	}
	return cursor, len(groups) < 64, nil
}
