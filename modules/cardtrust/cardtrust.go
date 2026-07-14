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
// Layering: this imports the table-backed modules/botidentity library only;
// robot, app_bot, and incomingwebhook do NOT import it, so there is no cycle.
// The `iwh_` prefix is re-declared here (production code must not cross-import
// modules/incomingwebhook per its display.go layering note); a test pins it to
// the exported contract constant.
package cardtrust

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/internal/resourceshare"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	resource_share "github.com/Mininglamp-OSS/octo-server/modules/resource_share"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
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
	// threat; a forged non-bot card is never cached as trusted because the
	// authoritative resolver returns no identity), so a generous window is fine.
	// Failed lookups are never cached.
	cacheTTL = 60 * time.Second
)

// botIdentityResolver is the minimal authoritative capability cardtrust needs.
// The consumer owns this narrow interface so tests can inject a deterministic
// resolver without depending on either lifecycle module.
type botIdentityResolver interface {
	Resolve(uid string) (*botidentity.Identity, error)
}

type verdict struct {
	trusted bool
	at      time.Time
}

// Resolver answers Trusted(fromUID). Construct ONCE per module (store on the
// handler/struct or a package singleton) so the LRU persists across the
// per-recipient push fan-out and across search pages — a large group's offline
// push then costs one identity query instead of one per recipient.
type Resolver struct {
	identity      botIdentityResolver
	cache         *lru.Cache[string, verdict]
	ttl           time.Duration
	log           *zap.Logger
	proofVerifier *resourceshare.ProofVerifier
}

type MessageObservation struct {
	FromUID   string
	ViewerUID string
	SpaceID   string
	Target    resourceshare.Target
	Payload   []byte
}

// New builds a Resolver backed by the authoritative bot identity resolver.
func New(ctx *config.Context) *Resolver {
	c, err := lruNew(cacheCapacity)
	if err != nil {
		// lru.New only errors on capacity <= 0; input is a constant, so this is
		// unreachable — fail loudly during init.
		panic(fmt.Sprintf("cardtrust: cache init: %v", err))
	}
	proofVerifier, err := resource_share.LoadProofVerifierFromEnv()
	if err != nil {
		panic(fmt.Sprintf("cardtrust: proof verifier init: %v", err))
	}
	return &Resolver{
		identity: botidentity.New(ctx), cache: c, ttl: cacheTTL, log: zap.L(),
		proofVerifier: proofVerifier,
	}
}

// lruNew wraps the typed LRU constructor so tests can build a Resolver with an
// injected identity resolver without going through New's botidentity.New(ctx).
func lruNew(capacity int) (*lru.Cache[string, verdict], error) {
	return lru.New[string, verdict](capacity)
}

// Trusted reports whether a type-17 message from fromUID may surface its stored
// plain. Webhook synthetic senders (iwh_ prefix) are trusted by construction;
// bot senders are resolved from active robot / published app_bot rows and cached.
// **Fail-closed**: on a lookup error the sender is treated as untrusted and the
// error verdict is NOT cached (so a transient DB blip cannot mask a legit bot's
// cards for the whole TTL).
func (r *Resolver) Trusted(fromUID string) bool {
	if r == nil || fromUID == "" {
		return false
	}
	if strings.HasPrefix(fromUID, webhookIDPrefix) {
		return true
	}
	if r.identity == nil || r.cache == nil {
		return false
	}
	if v, ok := r.cache.Get(fromUID); ok && (r.ttl <= 0 || time.Since(v.at) <= r.ttl) {
		return v.trusted
	}
	identity, err := r.identity.Resolve(fromUID)
	if err != nil {
		if r.log != nil {
			r.log.Warn("cardtrust: sender 身份查询失败,按不可信处理", zap.Error(err), zap.String("fromUID", fromUID))
		}
		return false
	}
	trusted := identity != nil
	r.cache.Add(fromUID, verdict{trusted: trusted, at: time.Now()})
	return trusted
}

// TrustedMessage preserves the established Bot/Webhook sender trust and adds
// one narrow human exception: a platform share proof must verify against the
// observed sender, Space and canonical channel target. Invalid JSON, missing
// context, oversized payloads and verifier outages all fail closed.
func (r *Resolver) TrustedMessage(observation MessageObservation) bool {
	if r == nil || observation.FromUID == "" {
		return false
	}
	if r.Trusted(observation.FromUID) {
		return true
	}
	if r.proofVerifier == nil || len(observation.Payload) == 0 || len(observation.Payload) > cardmsg.MaxPayloadBytes {
		return false
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(observation.Payload, &payload); err != nil || len(payload) == 0 {
		return false
	}
	if err := r.proofVerifier.Verify(payload, resourceshare.ProofObservation{
		ActorUID: observation.FromUID, ViewerUID: observation.ViewerUID,
		SpaceID: observation.SpaceID, Target: observation.Target,
	}); err != nil {
		return false
	}
	return true
}

func TargetFromChannel(channelID string, channelType uint8) (resourceshare.Target, bool) {
	switch channelType {
	case common.ChannelTypePerson.Uint8():
		if channelID == "" {
			return resourceshare.Target{}, false
		}
		return resourceshare.Target{Kind: resourceshare.TargetDM, PeerUID: channelID}, true
	case common.ChannelTypeGroup.Uint8():
		if channelID == "" {
			return resourceshare.Target{}, false
		}
		return resourceshare.Target{Kind: resourceshare.TargetGroup, GroupNo: channelID}, true
	case common.ChannelTypeCommunityTopic.Uint8():
		groupNo, shortID, err := thread.ParseChannelID(channelID)
		if err != nil {
			return resourceshare.Target{}, false
		}
		return resourceshare.Target{Kind: resourceshare.TargetThread, GroupNo: groupNo, ShortID: shortID}, true
	default:
		return resourceshare.Target{}, false
	}
}
