package readreceipt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func testSession(t *testing.T) (*dbr.Session, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() {
		if stats := db.Stats(); stats.InUse != 0 {
			t.Errorf("connection leaked: %+v", stats)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	conn := &dbr.Connection{DB: db, Dialect: dialect.MySQL, EventReceiver: &dbr.NullEventReceiver{}}
	return conn.NewSession(nil), mock
}

const insertPattern = `INSERT INTO member_readed`

func TestWriteNormalizesAtSQLBoundary(t *testing.T) {
	session, mock := testSession(t)
	rows := []Row{{MessageID: 7, UID: "z", ChannelID: "c", ChannelType: 2}, {MessageID: 6, UID: "z", ChannelID: "c", ChannelType: 2}, {MessageID: 6, UID: "a", ChannelID: "c", ChannelType: 2}, {MessageID: 6, UID: "z", ChannelID: "c", ChannelType: 2}}
	mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO member_readed .*VALUES \(6, 'c', 2, 'a'\),\(6, 'c', 2, 'z'\),\(7, 'c', 2, 'z'\) ON DUPLICATE KEY UPDATE message_id=VALUES\(message_id\)`).WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec("^COMMIT$").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := Write(context.Background(), session, rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].MessageID != 7 {
		t.Fatal("caller slice mutated")
	}
}

func TestWriteOrdersStoredDecimalStringsAcrossWidths(t *testing.T) {
	session, mock := testSession(t)
	rows := []Row{{MessageID: 9, UID: "u"}, {MessageID: 14, UID: "u"}, {MessageID: 10, UID: "u"}, {MessageID: 11, UID: "u"}, {MessageID: 12, UID: "u"}, {MessageID: 13, UID: "u"}, {MessageID: 10, UID: "u"}}
	mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("VALUES (10, '', 0, 'u'),(11, '', 0, 'u'),(12, '', 0, 'u'),(13, '', 0, 'u'),(14, '', 0, 'u'),(9, '', 0, 'u')")).WillReturnResult(sqlmock.NewResult(0, 6))
	mock.ExpectExec("^COMMIT$").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := Write(context.Background(), session, rows); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRetriesOnlyStatementDeadlock(t *testing.T) {
	deadlock := fmt.Errorf("wrapped: %w", &mysql.MySQLError{Number: 1213, Message: "deadlock"})
	for _, tc := range []struct {
		name                             string
		writeErr, commitErr, rollbackErr error
		attempts                         int
		success                          bool
	}{
		{"deadlock then success", deadlock, nil, nil, 2, true},
		{"lock timeout", &mysql.MySQLError{Number: 1205}, nil, nil, 1, false},
		{"network during statement", io.ErrUnexpectedEOF, nil, nil, 1, false},
		{"unknown commit", nil, io.ErrUnexpectedEOF, nil, 1, false},
		{"1213 at commit is not retried", nil, deadlock, nil, 1, false},
		{"failed rollback", deadlock, nil, io.ErrUnexpectedEOF, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, mock := testSession(t)
			mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
			if tc.writeErr != nil {
				mock.ExpectExec(insertPattern).WillReturnError(tc.writeErr)
				mock.ExpectExec("^ROLLBACK$").WillReturnResult(sqlmock.NewResult(0, 0)).WillReturnError(tc.rollbackErr)
			} else {
				mock.ExpectExec(insertPattern).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("^COMMIT$").WillReturnResult(sqlmock.NewResult(0, 0)).WillReturnError(tc.commitErr)
			}
			if tc.attempts == 2 {
				mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectExec(insertPattern).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("^COMMIT$").WillReturnResult(sqlmock.NewResult(0, 0))
			}
			writeReturned := false
			enqueued := 0
			err := AfterWrite(func() error {
				err := Write(context.Background(), session, []Row{{MessageID: 1, UID: "u", ChannelID: "c", ChannelType: 2}})
				writeReturned = true
				return err
			}, func() {
				if !writeReturned {
					t.Error("enqueue preceded Write return")
				}
				enqueued++
			})
			wantEnqueues := 0
			if tc.success {
				wantEnqueues = 1
			}
			if enqueued != wantEnqueues {
				t.Fatalf("enqueues=%d want=%d", enqueued, wantEnqueues)
			}
			if (err == nil) != tc.success {
				t.Fatalf("success=%v, error=%v", tc.success, err)
			}
			if !tc.success && tc.rollbackErr == nil {
				cause := tc.writeErr
				if cause == nil {
					cause = tc.commitErr
				}
				if !errors.Is(err, cause) {
					t.Fatalf("lost error cause: %v", err)
				}
			}
		})
	}
}

func TestWriteDeadlockExhaustion(t *testing.T) {
	session, mock := testSession(t)
	for i := 0; i < 3; i++ {
		mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(insertPattern).WillReturnError(&mysql.MySQLError{Number: 1213})
		mock.ExpectExec("^ROLLBACK$").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	err := Write(context.Background(), session, []Row{{MessageID: 1, UID: "u"}})
	var dbErr *mysql.MySQLError
	if !errors.As(err, &dbErr) || dbErr.Number != 1213 || !strings.Contains(err.Error(), "attempt 3") {
		t.Fatalf("expected exhausted deadlock, got %v", err)
	}
}

func TestWriteParentCancellationDoesNotDropReceipt(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired_deadline_%v", expired), func(t *testing.T) {
			session, mock := testSession(t)
			parent, cancel := context.WithCancel(context.Background())
			if expired {
				parent, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			}
			cancel()
			mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(insertPattern).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec("^COMMIT$").WillReturnResult(sqlmock.NewResult(0, 0))
			if err := Write(parent, session, []Row{{MessageID: 1}}); err != nil {
				t.Fatalf("parent cancellation dropped receipt: %v", err)
			}
		})
	}
}

func TestWriteRejectsConflictingDuplicate(t *testing.T) {
	session, _ := testSession(t)
	err := Write(context.Background(), session, []Row{{MessageID: 1, UID: "u", ChannelID: "a"}, {MessageID: 1, UID: "u", ChannelID: "b"}})
	if err == nil {
		t.Fatal("conflicting payload for one unique key accepted")
	}
}

func TestWriteBeginFailureAndEmpty(t *testing.T) {
	session, mock := testSession(t)
	if err := Write(context.Background(), session, nil); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0)).WillReturnError(io.EOF)
	if err := Write(context.Background(), session, []Row{{MessageID: 1}}); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v", err)
	}
}

func TestWriteTotalBudgetBoundsBegin(t *testing.T) {
	session, mock := testSession(t)
	mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0)).WillDelayFor(3 * time.Second)
	start := time.Now()
	err := writeWithPolicy(context.Background(), session, []Row{{MessageID: 1}}, 1, writeBudget)
	if err == nil {
		t.Fatal("delayed begin reported success")
	}
	if elapsed := time.Since(start); elapsed < writeBudget || elapsed > writeBudget+500*time.Millisecond {
		t.Fatalf("budget not enforced: %v", elapsed)
	}
}

func TestWriteCompletionRespectsContextAndDiscardsConnection(t *testing.T) {
	for _, phase := range []string{"COMMIT", "ROLLBACK"} {
		t.Run(phase, func(t *testing.T) {
			session, mock := testSession(t)
			mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
			if phase == "ROLLBACK" {
				mock.ExpectExec(insertPattern).WillReturnError(&mysql.MySQLError{Number: 1213})
			} else {
				mock.ExpectExec(insertPattern).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectExec("^" + phase + "$").WillDelayFor(time.Second).WillReturnResult(sqlmock.NewResult(0, 0))
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			start := time.Now()
			if err := writeWithPolicy(ctx, session, []Row{{MessageID: 1}}, 3, 150*time.Millisecond); err == nil {
				t.Fatal("timed-out completion reported success")
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("completion ignored deadline: %v", elapsed)
			}
			if stats := session.Connection.DB.Stats(); stats.OpenConnections != 0 {
				t.Fatalf("uncertain transaction returned to pool: %+v", stats)
			}
		})
	}
}

func TestWriteRejectsInvalidAttemptLimit(t *testing.T) {
	for _, attempts := range []int{0, -1} {
		session, _ := testSession(t)
		if err := writeWithPolicy(context.Background(), session, []Row{{MessageID: 1}}, attempts, writeBudget); err == nil {
			t.Fatal("invalid maxAttempts accepted")
		}
	}
}

func TestWriteAttemptBudgetAndFailureDiagnostics(t *testing.T) {
	for _, parentCanceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("parent_canceled_%v", parentCanceled), func(t *testing.T) {
			session, mock := testSession(t)
			mock.ExpectExec("^START TRANSACTION$").WillDelayFor(time.Second).WillReturnResult(sqlmock.NewResult(0, 0))
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			if parentCanceled {
				cancel()
			}
			start := time.Now()
			err := writeWithPolicy(parent, session, []Row{{MessageID: 1}}, 3, 300*time.Millisecond)
			if !errors.Is(err, ErrBudgetExceeded) || errors.Is(err, ErrParentCanceled) != parentCanceled {
				t.Fatalf("wrong error classification: %v", err)
			}
			if elapsed := time.Since(start); elapsed < 290*time.Millisecond || elapsed > 650*time.Millisecond {
				t.Fatalf("first-attempt floor/total budget not applied: %v", elapsed)
			}
		})
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	session, mock := testSession(t)
	mock.ExpectExec("^START TRANSACTION$").WillReturnError(io.EOF)
	err := Write(parent, session, []Row{{MessageID: 1}})
	if !errors.Is(err, ErrParentCanceled) || errors.Is(err, ErrBudgetExceeded) || !errors.Is(err, io.EOF) {
		t.Fatalf("parent cancellation confused with budget: %v", err)
	}
}

// A confirmed rollback can finish just before its child deadline, while the
// caller observes expiry just afterwards. Only the total budget vetoes retry.
type delayedRollbackReceiver struct{ dbr.NullEventReceiver }

func (*delayedRollbackReceiver) Event(name string) {
	if name == "dbr.rollback" {
		time.Sleep(550 * time.Millisecond)
	}
}
func TestWriteRetriesConfirmedRollbackAfterChildExpiry(t *testing.T) {
	session, mock := testSession(t)
	session.EventReceiver = &delayedRollbackReceiver{}
	mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(insertPattern).WillReturnError(&mysql.MySQLError{Number: 1213})
	mock.ExpectExec("^ROLLBACK$").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(insertPattern).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("^COMMIT$").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := writeWithPolicy(context.Background(), session, []Row{{MessageID: 1}}, 3, 1200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
func TestWriteInsufficientRetryBudget(t *testing.T) {
	session, mock := testSession(t)
	deadlock := &mysql.MySQLError{Number: 1213}
	mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(insertPattern).WillReturnError(deadlock)
	mock.ExpectExec("^ROLLBACK$").WillReturnResult(sqlmock.NewResult(0, 0))
	err := writeWithPolicy(context.Background(), session, []Row{{MessageID: 1}}, 3, 8*time.Millisecond)
	if !errors.Is(err, ErrBudgetExceeded) || !errors.Is(err, deadlock) || errors.Is(err, ErrParentCanceled) {
		t.Fatalf("budget/1213 classification: %v", err)
	}
}
func TestWriteBudgetExpiresDuringRetrySleep(t *testing.T) {
	session, mock := testSession(t)
	deadlock := &mysql.MySQLError{Number: 1213}
	mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(insertPattern).WillReturnError(deadlock)
	mock.ExpectExec("^ROLLBACK$").WillReturnResult(sqlmock.NewResult(0, 0))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Millisecond)
	defer cancel()
	retry, err := writeAttempt(ctx, session, []Row{{MessageID: 1}})
	if !retry || !errors.Is(err, deadlock) {
		t.Fatalf("fast 1213/confirmed rollback: %v %v", retry, err)
	}
	// Exercise the same wait used by Write deterministically: scheduler delays can
	// consume budget after admission. No fixed sleep or jitter-dependent outcome.
	waitErr := waitForRetry(ctx, 30*time.Millisecond)
	err = writeFailure(context.Background(), errors.Join(err, waitErr), waitErr != nil)
	if !errors.Is(err, ErrBudgetExceeded) || !errors.Is(err, deadlock) || errors.Is(err, ErrParentCanceled) {
		t.Fatalf("sleep budget classification: %v", err)
	}
}
