// Package cardtrust owns the single implementation of the "trusted card
// sender" predicate used by every server-authored display surface that must
// apply the Decision-2 residual-risk masking (card-message-protocol P1,
// round-3 P1-2): for a type-17 message, surface the stored `plain` only when
// the sender is a bot / webhook identity; otherwise mask to `[卡片]`.
//
// Why a shared package: the predicate is a security boundary and will evolve
// (a second synthetic-sender prefix, bot-status/space scoping, …). Keeping it
// in ONE place makes drift impossible — previously webhook and messages_search
// each carried a private copy, so a hardening in one surface silently left the
// other leaking attacker-controlled plain.
//
// Layering: this is a leaf that imports modules/robot only; robot and
// incomingwebhook do NOT import it, so there is no cycle. The `iwh_` prefix is
// re-declared here (production code must not cross-import modules/incomingwebhook
// per its display.go layering note); a test pins it to the exported contract
// constant.
package cardtrust

import (
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	lru "github.com/hashicorp/golang-lru/v2"
	"go.uber.org/zap"
)

// webhookIDPrefix is the incoming-webhook synthetic-sender UID prefix. Pinned
// to incomingwebhook.WebhookIDPrefix by cardtrust_test.go (compile-invisible
// drift is caught there).
const webhookIDPrefix = "iwh_"

const (
	// cacheCapacity bounds the in-process LRU (uid -> bot? verdict). Well under
	// a megabyte; the working set of distinct card senders is tiny.
	cacheCapacity = 4_096
	// cacheTTL soft-expiry. A masking verdict is not safety-critical to the
	// second (a deleted bot's own past cards showing plain briefly is not a
	// threat; a forged non-bot card is never cached as trusted because
	// ExistRobot returns false for it), so a generous window is fine. Fail-open
	// lookups (DB error) are never cached.
	cacheTTL = 60 * time.Second
)

// robotExister is the minimal robot capability cardtrust needs — narrowed from
// robot.IService so the resolver depends only on the one method it uses (and so
// tests can inject a stub without implementing the full service). *robot.Service
// satisfies it.
type robotExister interface {
	ExistRobot(uid string) (bool, error)
}

type verdict struct {
	trusted bool
	at      time.Time
}

// Resolver answers Trusted(fromUID). Construct ONCE per module (store on the
// handler/struct or a package singleton) so the LRU persists across the
// per-recipient push fan-out and across search pages — a large group's offline
// push then costs one ExistRobot query instead of one per recipient.
type Resolver struct {
	svc   robotExister
	cache *lru.Cache[string, verdict]
	ttl   time.Duration
	log   *zap.Logger
}

// New builds a Resolver backed by the read-only robot service.
func New(ctx *config.Context) *Resolver {
	c, err := lruNew(cacheCapacity)
	if err != nil {
		// lru.New only errors on capacity <= 0; input is a constant, so this is
		// unreachable — fail loudly during init.
		panic(fmt.Sprintf("cardtrust: cache init: %v", err))
	}
	return &Resolver{svc: robot.NewService(ctx), cache: c, ttl: cacheTTL, log: zap.L()}
}

// lruNew wraps the typed LRU constructor so tests can build a Resolver with an
// injected robot.IService without going through New's robot.NewService(ctx).
func lruNew(capacity int) (*lru.Cache[string, verdict], error) {
	return lru.New[string, verdict](capacity)
}

// Trusted reports whether a type-17 message from fromUID may surface its stored
// plain. Webhook synthetic senders (iwh_ prefix) are trusted by construction;
// bot senders are resolved via ExistRobot (active robots only) and cached.
// **Fail-closed**: on a lookup error the sender is treated as untrusted and the
// error verdict is NOT cached (so a transient DB blip cannot mask a legit bot's
// cards for the whole TTL).
func (r *Resolver) Trusted(fromUID string) bool {
	if strings.HasPrefix(fromUID, webhookIDPrefix) {
		return true
	}
	if r == nil {
		return false
	}
	if v, ok := r.cache.Get(fromUID); ok && (r.ttl <= 0 || time.Since(v.at) <= r.ttl) {
		return v.trusted
	}
	isBot, err := r.svc.ExistRobot(fromUID)
	if err != nil {
		if r.log != nil {
			r.log.Warn("cardtrust: sender 身份查询失败,按不可信处理", zap.Error(err), zap.String("fromUID", fromUID))
		}
		return false
	}
	r.cache.Add(fromUID, verdict{trusted: isBot, at: time.Now()})
	return isBot
}
