// Package readreceipt owns the final write boundary shared by user and Bot receipts.
package readreceipt

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
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
func Write(ctx context.Context, session *dbr.Session, rows []Row) error {
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := normalize(rows)
	if err != nil || len(rows) == 0 {
		return err
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		retry, err := writeAttempt(ctx, session, rows)
		if err == nil {
			return nil
		}
		err = fmt.Errorf("read receipt attempt %d: %w", attempt, err)
		if !retry || attempt == maxAttempts {
			return err
		}
		// Exponential 10/20 ms base plus independent jitter, bounded by ctx.
		base := (10 * time.Millisecond) << (attempt - 1)
		timer := time.NewTimer(base + time.Duration(rand.Int64N(int64(base))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %w", err, ctx.Err())
		case <-timer.C:
		}
	}
	panic("unreachable")
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
		messageID int64
		uid       string
	}
	unique := make(map[key]Row, len(rows))
	for _, row := range rows {
		k := key{row.MessageID, row.UID}
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
			return result[i].MessageID < result[j].MessageID
		}
		return result[i].UID < result[j].UID
	})
	return result, nil
}
