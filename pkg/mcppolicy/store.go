package mcppolicy

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/gocraft/dbr/v2"
)

const queryBudget = 2 * time.Second

// Store uses the application's existing pool. It has no cache, repair/seed
// path, retry, configuration broadcast, or fallback to a code default.
type Store struct{ Session *dbr.Session }

type defaultRow struct {
	mode       Mode
	generation uint64
}

type botRow struct {
	mode     Mode
	revision uint64
}

// OwnerCheck runs after the singleton lock and before the policy-row lock.
// The robot module supplies its existing creator decision inside this tx.
type OwnerCheck func(context.Context, *dbr.Tx) error

func (s Store) begin(ctx context.Context, readonly bool) (*dbr.Tx, error) {
	if s.Session == nil {
		return nil, ErrUnavailable
	}
	// Keep transaction lifecycle errors out of the shared receiver as well:
	// this API emits only fixed fault classes, never driver diagnostics.
	session := *s.Session
	session.EventReceiver = &dbr.NullEventReceiver{}
	tx, err := session.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: readonly})
	if err != nil {
		return nil, ErrUnavailable
	}
	return tx, nil
}

func readDefault(ctx context.Context, tx *dbr.Tx, lock bool) (defaultRow, error) {
	q := "SELECT context_mode, generation FROM mcp_policy_default WHERE singleton_id=1"
	if lock {
		q += " FOR UPDATE"
	}
	var row defaultRow
	// Use the embedded SQL transaction so dbr cannot log interpolated policy
	// parameters on errors. Diagnostics at the HTTP boundary are fixed classes.
	if err := tx.QueryRowContext(ctx, q).Scan(&row.mode, &row.generation); err != nil || !row.mode.Valid() || row.generation == 0 {
		return defaultRow{}, ErrUnavailable
	}
	return row, nil
}

func readBot(ctx context.Context, tx *dbr.Tx, uid string, def defaultRow, lock bool) (*botRow, error) {
	q := "SELECT context_mode, revision FROM mcp_bot_policy WHERE bot_uid=?"
	if lock {
		q += " FOR UPDATE"
	}
	var row botRow
	err := tx.QueryRowContext(ctx, q, uid).Scan(&row.mode, &row.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || !row.mode.Valid() || row.revision == 0 || row.revision > def.generation {
		return nil, ErrUnavailable
	}
	return &row, nil
}

func policy(def defaultRow, bot *botRow) Policy {
	p := Policy{Version: 1, ContextMode: def.mode, StageDefault: def.mode, Source: "stage_default", Generation: strconv.FormatUint(def.generation, 10)}
	if bot != nil {
		mode, revision := bot.mode, strconv.FormatUint(bot.revision, 10)
		p.ContextMode, p.Override, p.Source, p.BotRevision = mode, &mode, "bot", &revision
	}
	return p
}

func defaultPolicy(def defaultRow) DefaultPolicy {
	return DefaultPolicy{Version: 1, ContextMode: def.mode, Generation: strconv.FormatUint(def.generation, 10)}
}

// Resolve reads default then override in a single read-only consistent
// snapshot. Even an explicit override requires a valid mandatory default.
func (s Store) Resolve(ctx context.Context, authenticatedBotUID string) (Policy, error) {
	if !ValidUID(authenticatedBotUID) {
		return Policy{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, queryBudget)
	defer cancel()
	tx, err := s.begin(ctx, true)
	if err != nil {
		return Policy{}, err
	}
	defer tx.RollbackUnlessCommitted()
	def, err := readDefault(ctx, tx, false)
	if err != nil {
		return Policy{}, err
	}
	bot, err := readBot(ctx, tx, authenticatedBotUID, def, false)
	if err != nil {
		return Policy{}, err
	}
	if err := tx.Commit(); err != nil {
		return Policy{}, ErrUnavailable
	}
	return policy(def, bot), nil
}

func (s Store) GetDefault(ctx context.Context) (DefaultPolicy, error) {
	ctx, cancel := context.WithTimeout(ctx, queryBudget)
	defer cancel()
	tx, err := s.begin(ctx, true)
	if err != nil {
		return DefaultPolicy{}, err
	}
	defer tx.RollbackUnlessCommitted()
	def, err := readDefault(ctx, tx, false)
	if err != nil {
		return DefaultPolicy{}, err
	}
	if err := tx.Commit(); err != nil {
		return DefaultPolicy{}, ErrUnavailable
	}
	return defaultPolicy(def), nil
}

func (s Store) PutBot(ctx context.Context, uid, actor string, mode Mode, generation uint64, check OwnerCheck) (Policy, error) {
	if !mode.Valid() {
		return Policy{}, ErrInvalid
	}
	return s.writeBot(ctx, uid, actor, &mode, generation, check)
}

func (s Store) DeleteBot(ctx context.Context, uid, actor string, generation uint64, check OwnerCheck) (Policy, error) {
	return s.writeBot(ctx, uid, actor, nil, generation, check)
}

func (s Store) writeBot(ctx context.Context, uid, actor string, mode *Mode, generation uint64, check OwnerCheck) (Policy, error) {
	if !ValidUID(uid) || !ValidUID(actor) || generation == 0 || check == nil {
		return Policy{}, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, queryBudget)
	defer cancel()
	tx, err := s.begin(ctx, false)
	if err != nil {
		return Policy{}, err
	}
	defer tx.RollbackUnlessCommitted()
	def, err := readDefault(ctx, tx, true)
	if err != nil {
		return Policy{}, err
	}
	if def.generation != generation {
		return Policy{}, ErrConflict
	}
	if err := check(ctx, tx); err != nil {
		return Policy{}, err
	}
	bot, err := readBot(ctx, tx, uid, def, true)
	if err != nil {
		return Policy{}, err
	}
	changed := (mode == nil && bot != nil) || (mode != nil && (bot == nil || bot.mode != *mode))
	if changed {
		if def.generation == math.MaxUint64 {
			return Policy{}, ErrUnavailable
		}
		def.generation++
		if mode == nil {
			_, err = tx.ExecContext(ctx, "DELETE FROM mcp_bot_policy WHERE bot_uid=?", uid)
			bot = nil
		} else {
			_, err = tx.ExecContext(ctx, "INSERT INTO mcp_bot_policy (bot_uid, context_mode, revision, updated_by) VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE context_mode=VALUES(context_mode), revision=VALUES(revision), updated_by=VALUES(updated_by)", uid, *mode, def.generation, actor)
			bot = &botRow{mode: *mode, revision: def.generation}
		}
		if err != nil {
			return Policy{}, ErrUnavailable
		}
		if _, err = tx.ExecContext(ctx, "UPDATE mcp_policy_default SET generation=?, updated_by=? WHERE singleton_id=1", def.generation, actor); err != nil {
			return Policy{}, ErrUnavailable
		}
	}
	if err := tx.Commit(); err != nil {
		return Policy{}, ErrUnavailable
	}
	return policy(def, bot), nil
}

func (s Store) PutDefault(ctx context.Context, actor string, mode Mode, generation uint64) (DefaultPolicy, error) {
	if !ValidUID(actor) || !mode.Valid() || generation == 0 {
		return DefaultPolicy{}, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, queryBudget)
	defer cancel()
	tx, err := s.begin(ctx, false)
	if err != nil {
		return DefaultPolicy{}, err
	}
	defer tx.RollbackUnlessCommitted()
	def, err := readDefault(ctx, tx, true)
	if err != nil {
		return DefaultPolicy{}, err
	}
	if def.generation != generation {
		return DefaultPolicy{}, ErrConflict
	}
	if def.mode != mode {
		if def.generation == math.MaxUint64 {
			return DefaultPolicy{}, ErrUnavailable
		}
		def.mode, def.generation = mode, def.generation+1
		if _, err := tx.ExecContext(ctx, "UPDATE mcp_policy_default SET context_mode=?, generation=?, updated_by=? WHERE singleton_id=1", mode, def.generation, actor); err != nil {
			return DefaultPolicy{}, ErrUnavailable
		}
	}
	if err := tx.Commit(); err != nil {
		return DefaultPolicy{}, ErrUnavailable
	}
	return defaultPolicy(def), nil
}
