package mcppolicy

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func mockStore(t *testing.T) (Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return Store{Session: conn.NewSession(nil)}, mock
}

func expectDefault(mock sqlmock.Sqlmock, mode string, gen any) {
	mock.ExpectQuery("SELECT context_mode, generation FROM mcp_policy_default").WillReturnRows(sqlmock.NewRows([]string{"context_mode", "generation"}).AddRow(mode, gen))
}

func expectBot(mock sqlmock.Sqlmock, mode string, rev any) {
	rows := sqlmock.NewRows([]string{"context_mode", "revision"})
	if mode != "" {
		rows.AddRow(mode, rev)
	}
	mock.ExpectQuery("SELECT context_mode, revision FROM mcp_bot_policy").WithArgs("bot_B").WillReturnRows(rows)
}

var allowOwner OwnerCheck = func(context.Context, *dbr.Tx) error { return nil }

func TestResolveSnapshot(t *testing.T) {
	for _, override := range []bool{false, true} {
		s, mock := mockStore(t)
		mock.ExpectBegin()
		expectDefault(mock, "strict", 7)
		if override {
			expectBot(mock, "compatible", 6)
		} else {
			expectBot(mock, "", nil)
		}
		mock.ExpectCommit()
		p, err := s.Resolve(context.Background(), "bot_B")
		if err != nil {
			t.Fatal(err)
		}
		if p.Generation != "7" || p.StageDefault != Strict {
			t.Fatalf("%+v", p)
		}
		if override {
			if p.Source != "bot" || p.ContextMode != Compatible || *p.BotRevision != "6" {
				t.Fatalf("%+v", p)
			}
		} else if p.Source != "stage_default" || p.Override != nil || p.BotRevision != nil || p.ContextMode != Strict {
			t.Fatalf("%+v", p)
		}
	}
}

func TestResolveFailures(t *testing.T) {
	for _, fault := range []string{"begin", "default_query", "missing_default", "invalid_default", "zero_generation", "scan", "bot_query", "invalid_bot", "zero_revision", "future_revision", "commit"} {
		t.Run(fault, func(t *testing.T) {
			s, mock := mockStore(t)
			if fault == "begin" {
				mock.ExpectBegin().WillReturnError(errors.New("private driver cause"))
			} else {
				mock.ExpectBegin()
				switch fault {
				case "default_query":
					mock.ExpectQuery("SELECT context_mode, generation").WillReturnError(errors.New("missing table"))
				case "missing_default":
					mock.ExpectQuery("SELECT context_mode, generation").WillReturnRows(sqlmock.NewRows([]string{"context_mode", "generation"}))
				case "invalid_default":
					expectDefault(mock, "STRICT", 7)
				case "zero_generation":
					expectDefault(mock, "strict", 0)
				case "scan":
					expectDefault(mock, "strict", "not-a-number")
				default:
					expectDefault(mock, "strict", 7)
					switch fault {
					case "bot_query":
						mock.ExpectQuery("SELECT context_mode, revision").WillReturnError(errors.New("private query failure"))
					case "invalid_bot":
						expectBot(mock, "strict ", 6)
					case "zero_revision":
						expectBot(mock, "strict", 0)
					case "future_revision":
						expectBot(mock, "strict", 8)
					case "commit":
						expectBot(mock, "compatible", 6)
					}
				}
				if fault == "commit" {
					mock.ExpectCommit().WillReturnError(errors.New("private commit cause"))
				} else {
					mock.ExpectRollback()
				}
			}
			if _, err := s.Resolve(context.Background(), "bot_B"); err != ErrUnavailable {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestWriteBotCASAndNoOps(t *testing.T) {
	for _, tc := range []string{"conflict", "owner_denied", "same", "delete_absent", "insert_equal_default", "delete", "update_failure", "generation_failure", "commit_failure", "overflow"} {
		t.Run(tc, func(t *testing.T) {
			s, mock := mockStore(t)
			mock.ExpectBegin()
			gen := uint64(7)
			if tc == "overflow" {
				gen = math.MaxUint64
			}
			expectDefault(mock, "compatible", genString(gen))
			check := allowOwner
			ownerErr := errors.New("owner denied")
			if tc == "owner_denied" {
				check = func(context.Context, *dbr.Tx) error { return ownerErr }
			}
			want := error(nil)
			if tc == "conflict" {
				want = ErrConflict
			} else if tc == "owner_denied" {
				want = ownerErr
			} else {
				switch tc {
				case "same", "delete", "update_failure", "generation_failure", "commit_failure":
					expectBot(mock, "compatible", 6)
				default:
					expectBot(mock, "", nil)
				}
				if tc == "overflow" {
					want = ErrUnavailable
				} else if tc != "same" && tc != "delete_absent" {
					q := "INSERT INTO mcp_bot_policy"
					if tc == "delete" {
						q = "DELETE FROM mcp_bot_policy"
					}
					if tc == "update_failure" {
						mock.ExpectExec(q).WillReturnError(errors.New("write failed"))
						want = ErrUnavailable
					} else {
						mock.ExpectExec(q).WillReturnResult(sqlmock.NewResult(0, 1))
						if tc == "generation_failure" {
							mock.ExpectExec("UPDATE mcp_policy_default SET generation").WillReturnError(errors.New("generation failed"))
							want = ErrUnavailable
						} else {
							mock.ExpectExec("UPDATE mcp_policy_default SET generation").WithArgs(uint64(8), "owner_U").WillReturnResult(sqlmock.NewResult(0, 1))
						}
					}
				}
			}
			if want != nil {
				mock.ExpectRollback()
			} else if tc == "commit_failure" {
				mock.ExpectCommit().WillReturnError(errors.New("uncertain commit"))
				want = ErrUnavailable
			} else {
				mock.ExpectCommit()
			}
			if tc == "conflict" {
				gen = 6
			}
			var p Policy
			var err error
			if tc == "delete" || tc == "delete_absent" {
				p, err = s.DeleteBot(context.Background(), "bot_B", "owner_U", gen, check)
			} else {
				mode := Strict
				if tc == "same" || tc == "insert_equal_default" {
					mode = Compatible
				}
				p, err = s.PutBot(context.Background(), "bot_B", "owner_U", mode, gen, check)
			}
			if err != want {
				t.Fatalf("err %v, want %v", err, want)
			}
			if err == nil {
				wantGen := "8"
				if tc == "same" || tc == "delete_absent" {
					wantGen = "7"
				}
				if p.Generation != wantGen {
					t.Fatalf("%+v", p)
				}
				if tc == "insert_equal_default" && (p.Override == nil || *p.Override != Compatible) {
					t.Fatal("explicit default-equivalent override lost")
				}
			}
		})
	}
}

func TestWriteDefaultCAS(t *testing.T) {
	for _, tc := range []string{"change", "same", "stale", "overflow"} {
		t.Run(tc, func(t *testing.T) {
			s, mock := mockStore(t)
			mock.ExpectBegin()
			gen := uint64(7)
			if tc == "overflow" {
				gen = math.MaxUint64
			}
			expectDefault(mock, "compatible", genString(gen))
			mode := Strict
			want := error(nil)
			switch tc {
			case "same":
				mode = Compatible
				mock.ExpectCommit()
			case "stale":
				gen = 6
				want = ErrConflict
				mock.ExpectRollback()
			case "overflow":
				want = ErrUnavailable
				mock.ExpectRollback()
			default:
				mock.ExpectExec("UPDATE mcp_policy_default SET context_mode").WithArgs(Strict, uint64(8), "admin_U").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			}
			p, err := s.PutDefault(context.Background(), "admin_U", mode, gen)
			if err != want {
				t.Fatalf("%v != %v", err, want)
			}
			if tc == "change" && (p.ContextMode != Strict || p.Generation != "8") {
				t.Fatalf("%+v", p)
			}
		})
	}
}

func TestResolveDeadlineAndCancellation(t *testing.T) {
	s, mock := mockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT context_mode, generation").WillDelayFor(50 * time.Millisecond).WillReturnRows(sqlmock.NewRows([]string{"context_mode", "generation"}).AddRow("strict", 7))
	mock.ExpectRollback()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := s.Resolve(ctx, "bot_B"); err != ErrUnavailable {
		t.Fatal(err)
	}
	if _, err := (Store{}).Resolve(context.Background(), "bot_B"); err != ErrUnavailable {
		t.Fatal(err)
	}
}

func genString(n uint64) string {
	return policy(defaultRow{mode: Compatible, generation: n}, nil).Generation
}
