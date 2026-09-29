package imreconcile

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

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
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if readErr != nil {
			return readErr
		}
		return snapshotResponseError(resp.StatusCode, body)
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
	err := w.db.QueryRowContext(c, "SELECT revision FROM im_group_reconcile WHERE group_no=?", group).Scan(&wanted)
	if errors.Is(err, sql.ErrNoRows) {
		// During initial adoption a no-op caller can arrive before the scanner.
		// Capture real authority; absence of a queue row does not prove IM agrees.
		if err = w.adoptGroup(c, group); err == nil {
			err = w.db.QueryRowContext(c, "SELECT revision FROM im_group_reconcile WHERE group_no=?", group).Scan(&wanted)
		}
	}
	if err != nil {
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

func (w *Worker) adoptGroup(ctx context.Context, group string) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var canonical string
	if err := tx.QueryRowContext(ctx, "SELECT group_no FROM `group` WHERE group_no=? FOR UPDATE", group).Scan(&canonical); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO im_group_reconcile (group_no,revision,pending,next_attempt_at)
		VALUES (?,1,1,UTC_TIMESTAMP(6)) ON DUPLICATE KEY UPDATE group_no=group_no`, canonical); err != nil {
		return err
	}
	return tx.Commit()
}

type page struct {
	group          string
	revision       uint64 // immutable active revision
	desired        uint64 // observed latest business revision, for claim-error CAS
	observedActive uint64
	owner          string
	attempts       uint32
	snapshots      []channelWork
}

func (w *Worker) step(ctx context.Context, group string) (bool, error) {
	select {
	case groupSlots <- struct{}{}:
		defer func() { <-groupSlots }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	p, err := w.claim(ctx, group)
	if err != nil {
		return false, errors.Join(err, w.recordClaimError(p, err))
	}
	if p == nil {
		return false, nil
	}
	// Claim committed. No SQL locks span HTTP; every request uses stored data.
	results := make([]error, len(p.snapshots))
	var wg sync.WaitGroup
	for i, snapshot := range p.snapshots {
		wg.Add(1)
		go func(i int, snapshot channelWork) {
			defer wg.Done()
			results[i] = w.send(ctx, snapshot.Snapshot)
		}(i, snapshot)
	}
	wg.Wait()
	changed, finishErr := w.checkpoint(p, results)
	return changed, errors.Join(errors.Join(results...), finishErr)
}

func (w *Worker) claim(ctx context.Context, group string) (*page, error) {
	tx, err := w.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p := &page{group: group}
	var claimable bool
	err = tx.QueryRowContext(ctx, `SELECT group_no,revision,active_revision,attempts,
		pending=1 AND next_attempt_at<=UTC_TIMESTAMP(6)
		AND (lease_until IS NULL OR lease_until<=UTC_TIMESTAMP(6))
		FROM im_group_reconcile WHERE group_no=? FOR UPDATE`, group).
		Scan(&p.group, &p.desired, &p.observedActive, &p.attempts, &claimable)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return p, err
	}
	if !claimable {
		return nil, nil
	}
	p.revision = p.observedActive
	if p.revision != 0 && p.desired > p.revision && p.attempts >= 3 {
		// A healthy snapshot is never restarted by churn. But an obsolete
		// snapshot that repeatedly fails must not indefinitely block a newer
		// compensation (e.g. deleting a group whose create reply was lost).
		// Yield only after durable backoff, with the group lease available.
		// All later sends use the newer receiver-fenced revision.
		if err = deleteSnapshot(ctx, tx, p.group); err != nil {
			return p, err
		}
		p.revision, p.attempts = 0, 0
	}
	if p.revision == 0 {
		p.revision = p.desired
		if err = captureSnapshot(ctx, tx, p.group, p.revision); err != nil {
			return p, err
		}
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return p, err
	}
	p.owner = hex.EncodeToString(token[:])
	if _, err = tx.ExecContext(ctx, `UPDATE im_group_reconcile SET active_revision=?,lease_owner=?,attempts=?,
		lease_until=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 30 SECOND) WHERE group_no=?`,
		p.revision, p.owner, p.attempts, p.group); err != nil {
		return p, err
	}
	p.snapshots, err = loadSnapshotWork(ctx, tx, p.group, p.revision)
	if err != nil {
		return p, err
	}
	if err = tx.Commit(); err != nil {
		return p, err
	}
	return p, nil
}

// A failed claim rolls back its lease and capture. Update only the exact
// observed generation while it remains unleased; never delay a newer mutation
// or steal the lease of a worker that claimed after our rollback.
func (w *Worker) recordClaimError(p *page, cause error) error {
	if p == nil || p.desired == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := w.db.ExecContext(ctx, `UPDATE im_group_reconcile SET attempts=attempts+1,
		next_attempt_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL ? SECOND),last_error=?
		WHERE group_no=? AND revision=? AND active_revision=? AND pending=1
		AND (lease_until IS NULL OR lease_until<=UTC_TIMESTAMP(6))`,
		retrySeconds(p.attempts), errorMessage(cause), p.group, p.desired, p.observedActive)
	return err
}

func retrySeconds(attempts uint32) int64 { return int64(1 << min(attempts, 8)) }

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	message := []rune(err.Error())
	return string(message[:min(len(message), 512)])
}

func (w *Worker) checkpoint(p *page, results []error) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var active uint64
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT active_revision,lease_owner FROM im_group_reconcile
		WHERE group_no=? FOR UPDATE`, p.group).Scan(&active, &owner); err != nil {
		return false, err
	}
	if active != p.revision || owner != p.owner {
		return false, nil
	}
	for _, result := range results {
		var response *snapshotHTTPError
		if errors.As(result, &response) && response.Status == http.StatusConflict && response.Code == "snapshot_upgrade_required" {
			// Only this receiver-declared upgrade condition permits a new revision.
			// Concurrent responses can reset once: this lease/active CAS fences all
			// old senders. Ordinary 409 conflicts remain visible and retryable.
			if err = deleteSnapshot(ctx, tx, p.group); err != nil {
				return false, err
			}
			_, err = tx.ExecContext(ctx, `UPDATE im_group_reconcile SET revision=GREATEST(revision,active_revision)+1,
				active_revision=0,child_cursor=0,member_page=0,pending=1,attempts=0,
				lease_owner='',lease_until=NULL,last_error=?,next_attempt_at=UTC_TIMESTAMP(6)
				WHERE group_no=?`, errorMessage(result), p.group)
			if err != nil {
				return false, err
			}
			return true, tx.Commit()
		}
	}
	progress := false
	for i, snapshot := range p.snapshots {
		if results[i] != nil {
			continue
		}
		r, err := tx.ExecContext(ctx, `UPDATE im_reconcile_snapshot_channel SET member_page=member_page+1,complete=(member_page=page_count)
			WHERE group_no=? AND revision=? AND child_id=? AND member_page=?`,
			p.group, p.revision, snapshot.ChildID, snapshot.PageIndex)
		if err != nil {
			return false, err
		}
		changed, err := r.RowsAffected()
		if err != nil {
			return false, err
		}
		progress = progress || changed > 0
		if snapshot.Deleted && snapshot.ChildID > 0 && snapshot.PageIndex+1 == snapshot.PageCount {
			// Retain the historical identity, but stop replaying confirmed physical
			// deletions on every unrelated future group mutation.
			if _, err = tx.ExecContext(ctx, `UPDATE im_reconcile_deleted_channel SET completed_revision=?
				WHERE group_no=? AND thread_id=? AND BINARY channel_id=BINARY ? AND completed_revision=0`,
				p.revision, p.group, snapshot.ChildID, snapshot.ChannelID); err != nil {
				return false, err
			}
		}
	}
	var child int64
	var member uint32
	err = tx.QueryRowContext(ctx, `SELECT child_id,member_page FROM im_reconcile_snapshot_channel
		WHERE group_no=? AND revision=? AND complete=0 ORDER BY child_id LIMIT 1`, p.group, p.revision).Scan(&child, &member)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `UPDATE im_group_reconcile SET completed_revision=GREATEST(completed_revision,active_revision),
			pending=(revision>active_revision),active_revision=0,child_cursor=0,member_page=0,
			lease_owner='',lease_until=NULL,attempts=0,last_error='',next_attempt_at=UTC_TIMESTAMP(6) WHERE group_no=?`, p.group)
		if err != nil {
			return false, err
		}
		if err = deleteSnapshot(ctx, tx, p.group); err != nil {
			return false, err
		}
	} else {
		if err != nil {
			return false, err
		}
		cause := errors.Join(results...)
		delay, attempts := int64(0), uint32(0)
		if cause != nil {
			delay, attempts = retrySeconds(p.attempts), p.attempts+1
		}
		_, err = tx.ExecContext(ctx, `UPDATE im_group_reconcile SET child_cursor=?,member_page=?,lease_owner='',lease_until=NULL,
			attempts=?,last_error=?,next_attempt_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL ? SECOND) WHERE group_no=?`,
			child, member, attempts, errorMessage(cause), delay, p.group)
		if err != nil {
			return false, err
		}
	}
	return progress, tx.Commit()
}

type snapshotHTTPError struct {
	Status int
	Code   string
}

func (e *snapshotHTTPError) Error() string {
	return fmt.Sprintf("IM subscriber reconciliation returned HTTP %d: %s", e.Status, e.Code)
}
func snapshotResponseError(status int, body []byte) error {
	if status == http.StatusOK {
		return nil
	}
	var response struct {
		Message string `json:"msg"`
		Data    struct {
			Error string `json:"error"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &response)
	code := response.Data.Error
	if code == "" {
		code = response.Message
	}
	return &snapshotHTTPError{Status: status, Code: code}
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
