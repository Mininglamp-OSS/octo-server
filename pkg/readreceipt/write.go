// Package readreceipt owns the final write boundary shared by user and Bot receipts.
package readreceipt

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
)

const writeBudget = 2 * time.Second
const maxAttempts = 3

// Row is one receipt. Callers supply the existing normalized channel identity.
type Row struct {
	MessageID   int64
	UID         string
	ChannelID   string
	ChannelType uint8
}

// Write returns nil only after a definite commit (or an empty batch). It preserves
// the existing no-op upsert, so overlapping requests cannot increment a count.
// Callers must enqueue Redis/aggregation work only after this function succeeds.
func Write(parent context.Context, session *dbr.Session, rows []Row) error {
	return writeWithPolicy(parent, session, rows, maxAttempts, writeBudget)
}

// AfterWrite gates one enqueue pass on the actual persistence return value.
// The enqueue callback may schedule asynchronous work, but is never retried.
func AfterWrite(write func() error, enqueue func()) error {
	if err := write(); err != nil {
		return err
	}
	enqueue()
	return nil
}

var (
	// ErrBudgetExceeded marks expiration of a total or per-attempt write budget.
	ErrBudgetExceeded = errors.New("budget_exceeded")
	// ErrParentCanceled is diagnostic on an independent write failure only.
	// Parent cancellation does not abort persistence or turn a commit into failure.
	ErrParentCanceled = errors.New("parent_canceled")
)

func writeWithPolicy(parent context.Context, session *dbr.Session, rows []Row, attempts int, budget time.Duration) error {
	if attempts <= 0 {
		return errors.New("read receipt maxAttempts must be positive")
	}
	// Intentional durability policy: disconnect/deadline must not drop an accepted
	// receipt. Preserve context values, but bound persistence independently.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), budget)
	defer cancel()
	rows, err := normalize(rows)
	if err != nil || len(rows) == 0 {
		return err
	}
	deadline, _ := ctx.Deadline()
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return writeFailure(parent, err, true)
		}
		remaining := time.Until(deadline)
		attemptCtx, finish := context.WithTimeout(ctx, remaining/time.Duration(attempts-attempt+1))
		retry, err := writeAttempt(attemptCtx, session, rows)
		exceeded := attemptCtx.Err() != nil
		finish()
		if err == nil {
			return nil
		}
		err = fmt.Errorf("read receipt attempt %d: %w", attempt, err)
		if !retry || attempt == attempts || exceeded {
			return writeFailure(parent, err, exceeded)
		}
		// Exponential 10/20 ms base plus jitter, bounded by the total budget.
		base := (10 * time.Millisecond) << (attempt - 1)
		timer := time.NewTimer(base + time.Duration(rand.Int64N(int64(base))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return writeFailure(parent, errors.Join(err, ctx.Err()), true)
		case <-timer.C:
		}
	}
	return errors.New("read receipt attempts exhausted")
}

func writeFailure(parent context.Context, err error, exceeded bool) error {
	if exceeded {
		err = errors.Join(ErrBudgetExceeded, context.DeadlineExceeded, err)
	}
	if parent.Err() != nil {
		err = fmt.Errorf("%w (ignored for persistence): %w", ErrParentCanceled, err)
	}
	return err
}

func writeAttempt(ctx context.Context, session *dbr.Session, rows []Row) (bool, error) {
	values := make([]string, 0, len(rows))
	args := make([]interface{}, 0, 4*len(rows))
	for _, row := range rows {
		values = append(values, "(?, ?, ?, ?)")
		args = append(args, row.MessageID, row.ChannelID, row.ChannelType, row.UID)
	}
	stmt := `INSERT INTO member_readed (message_id, channel_id, channel_type, uid) VALUES ` + strings.Join(values, ",") + ` ON DUPLICATE KEY UPDATE message_id=VALUES(message_id)`
	stmt, err := dbr.InterpolateForDialect(stmt, args, session.Dialect)
	if err != nil {
		return false, fmt.Errorf("build: %w", err)
	}
	// mysql v1.7.1's native Tx.Commit/Rollback do not watch context. Pin one
	// connection and execute every transaction command with ExecContext instead.
	conn, err := session.Connection.DB.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	reusable := false
	defer func() {
		if !reusable {
			// Never return a possibly active/unconfirmed transaction to the pool,
			// including on panic. ErrBadConn makes database/sql discard it.
			_ = conn.Raw(func(interface{}) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, "START TRANSACTION"); err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	session.Event("dbr.begin")
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
			return false, fmt.Errorf("write and rollback failed (connection discarded): %w", errors.Join(err, rollbackErr))
		}
		reusable = true
		session.Event("dbr.rollback")
		var mysqlErr *mysql.MySQLError
		deadlock := errors.As(err, &mysqlErr) && mysqlErr.Number == 1213
		return deadlock, fmt.Errorf("write (transaction rolled back): %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, fmt.Errorf("commit failed (outcome unconfirmed; connection discarded; not retried): %w", err)
	}
	reusable = true
	session.Event("dbr.commit")
	return false, nil
}

func normalize(rows []Row) ([]Row, error) {
	// Use exactly the same tuple for deduplication and the total sort order.
	// Reject conflicting payloads rather than choose based on arrival order.
	type key struct {
		messageID string
		uid       string
	}
	unique := make(map[key]Row, len(rows))
	for _, row := range rows {
		k := key{strconv.FormatInt(row.MessageID, 10), row.UID}
		if previous, ok := unique[k]; ok && previous != row {
			return nil, errors.New("read receipt has conflicting channels for one message/user key")
		}
		unique[k] = row
	}
	result := make([]Row, 0, len(unique))
	for _, row := range unique {
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].MessageID != result[j].MessageID {
			return strconv.FormatInt(result[i].MessageID, 10) < strconv.FormatInt(result[j].MessageID, 10)
		}
		return result[i].UID < result[j].UID
	})
	return result, nil
}
