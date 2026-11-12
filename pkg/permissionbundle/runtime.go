package permissionbundle

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

const runtimeKey = "permission-bundle/v1/runtime"

// Runtime is installed once, before module construction. Dependencies come from
// trusted server composition, never an HTTP request or a fallback catalog.
type Runtime struct {
	store               Store
	pin                 CatalogPin
	facts               NativeFactSource
	associationContexts sync.Map // *dbr.Tx -> its original bounded context
}

func Install(ctx *config.Context, store Store, pin CatalogPin, facts NativeFactSource) error {
	if ctx == nil || ctx.Value(runtimeKey) != nil || store.Session == nil || !ValidOrigin(store.Origin) || store.Authority == nil || store.Native == nil {
		return ErrUnavailable
	}
	c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, e := loadRelease(c, store.Releases, pin); e != nil {
		return e
	}
	ctx.SetValue(&Runtime{store: store, pin: pin, facts: facts}, runtimeKey)
	return nil
}
func RuntimeFor(ctx *config.Context) *Runtime {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(runtimeKey).(*Runtime)
	return r
}
func (r *Runtime) Store() *Store {
	if r == nil {
		return nil
	}
	return &r.store
}
func (r *Runtime) HasFacts() bool { return r != nil && r.facts != nil }
func (r *Runtime) HasDeliveryBinding() bool {
	if r == nil || r.facts == nil {
		return false
	}
	binder, ok := r.facts.(DeliveredMessageBinder)
	return ok && binder.HasDeliveryBinding()
}

// BeginAssociation keeps legacy transactions unchanged until composition is
// installed. Enabled native units use one fresh RR transaction and one deadline
// from before their native prefix through the final policy suffix.
func BeginAssociation(c *config.Context, session *dbr.Session) (*dbr.Tx, context.CancelFunc, error) {
	return BeginAssociationContext(context.Background(), c, session)
}

// BeginAssociationContext also carries cancellation for native callers with an
// existing request context. Both entry points retain one transaction deadline.
func BeginAssociationContext(parent context.Context, c *config.Context, session *dbr.Session) (*dbr.Tx, context.CancelFunc, error) {
	if session == nil {
		return nil, func() {}, ErrUnavailable
	}
	r := RuntimeFor(c)
	if r == nil {
		tx, e := session.Begin()
		return tx, func() {}, e
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	tx, e := session.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if e != nil {
		cancel()
		return nil, func() {}, e
	}
	r.associationContexts.Store(tx, ctx)
	return tx, func() { r.associationContexts.Delete(tx); cancel() }, nil
}

type Association struct {
	BotUID  string
	SpaceID string
	New     bool
}

// CommitAssociations is the final suffix of an existing native transaction.
// Native/project steps and partial-success units remain with their old owners.
// Nothing acquires a new native row after the first permission-header lock.
// Without an installed runtime, legacy native transactions retain their path;
// permission routes are not mounted in that composition.
func CommitAssociations(c *config.Context, tx *dbr.Tx, associations ...Association) error {
	if tx == nil {
		return ErrUnavailable
	}
	r := RuntimeFor(c)
	if r == nil {
		return tx.Commit()
	}
	ctx, ok := r.associationContexts.Load(tx)
	if !ok {
		return ErrUnavailable
	}
	return r.commitAssociations(ctx.(context.Context), tx, associations)
}
func (r *Runtime) commitAssociations(ctx context.Context, tx *dbr.Tx, associations []Association) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if tx == nil {
		return ErrUnavailable
	}
	a, e := r.store.Authority.Begin(ctx, tx)
	if e != nil || a == nil || !validHex(a.Incarnation(), 32) {
		return ErrUnavailable
	}
	keys := map[Key]bool{}
	for _, ref := range associations {
		key := Key{r.store.Origin, ref.BotUID, ref.SpaceID}
		if !key.Valid() {
			return ErrUnavailable
		}
		keys[key] = keys[key] || ref.New
	}
	ordered := make([]Key, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].BotUID != ordered[j].BotUID {
			return ordered[i].BotUID < ordered[j].BotUID
		}
		return ordered[i].SpaceID < ordered[j].SpaceID
	})
	anchors := map[Key]NativeAnchor{}
	for _, key := range ordered {
		// Classify only the supplied member, with bounded unique-key probes.
		// A human or built-in/nonhuman association creates no User Bot policy.
		q := `SELECT robot FROM user WHERE uid=?`
		if !r.store.testNoLocks {
			q += " FOR SHARE NOWAIT"
		}
		var robot int
		e = queryOne(ctx, tx, q, []any{key.BotUID}, &robot)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnavailable
		}
		if e != nil {
			return ErrUnavailable
		}
		if robot != 1 {
			continue
		}
		q = `SELECT creator_uid,COALESCE(bot_token,'') FROM robot WHERE robot_id=?`
		if !r.store.testNoLocks {
			q += " FOR SHARE NOWAIT"
		}
		var creator, token string
		e = queryOne(ctx, tx, q, []any{key.BotUID}, &creator, &token)
		if e != nil {
			return ErrUnavailable
		}
		if creator == "" || !strings.HasPrefix(token, "bf_") {
			continue
		}
		anchor, e := r.store.Native.Scope(ctx, tx, key, "", a.Incarnation())
		if e != nil {
			return e
		}
		if !validAnchor(anchor) {
			return ErrUnavailable
		}
		anchors[key] = anchor
	}
	// Final header suffix: every supported scope has its complete native locks.
	for _, key := range ordered {
		if anchor, ok := anchors[key]; ok {
			if keys[key] {
				e = r.store.ProvisionSuffix(ctx, tx, key, anchor, r.pin)
			} else {
				_, e = r.store.read(ctx, &scopeTx{tx: tx, key: key, anchor: anchor}, "SHARE")
			}
			if e != nil {
				return e
			}
		}
	}
	return complete(ctx, &scopeTx{tx: tx, authority: a})
}
