// Package botevent holds the bot-event "doorbell" primitive shared by the
// producer side (modules/robot enqueue chokepoints) and the consumer side
// (modules/bot_api long-poll on POST /v1/bot/events).
//
// It is a leaf package on purpose: modules/bot_api already imports
// modules/robot, so the key format cannot live in either module without
// creating an import cycle or a second copy that can drift.
//
// # What the doorbell is (and is not)
//
// The doorbell is a **hint that something was enqueued**, never the event
// itself. The authoritative queue remains the `robotEvent:{robotID}` sorted set
// read by ZRangeByScore from the caller's cursor. A consumer that wakes on the
// bell still performs that authoritative read; a consumer that never sees a
// bell simply waits out its hold and returns an empty batch, and the next poll
// picks the event up from the sorted set.
//
// That asymmetry is the whole safety argument: a lost, stolen, or stale bell
// can only cost latency, never an event. Nothing in this package may be
// promoted into a delivery path.
package botevent

import (
	"strings"
	"time"

	rd "github.com/go-redis/redis"
)

// BellKeyPrefix namespaces the per-bot doorbell list. Kept distinct from
// `robotEvent:` so the authoritative queue and the hint can never collide.
const BellKeyPrefix = "robotEventBell:"

// BellTTL bounds how long an unread doorbell token survives. It only has to
// outlive one long-poll hold (30s ceiling) — a bell that expires while an event
// still sits in the queue costs nothing, because consumers read the sorted set
// before they ever wait. Five minutes is slack, not a requirement.
const BellTTL = 5 * time.Minute

// ringSource performs the whole ring in one round trip.
//
// Why a script and not three calls: the ring sits on the ordinary message
// delivery path (saveRobotMessage), which already spends GenSeq + ZADD + EXPIRE.
// Issuing LPUSH, LTRIM and EXPIRE separately measured 256µs per ring against a
// loopback Redis and roughly *doubled* the per-message round trips on the
// hottest bot path — a cost every deployment pays today, since `wait` is opt-in
// and no shipped client sends it yet. One EVAL is also atomic, so the push and
// the trim cannot interleave with another producer's.
//
// LTRIM runs after LPUSH so the list is never observed empty by a waiter that is
// about to block: a ring landing between a consumer's read and its BLPOP leaves
// a token behind, and the BLPOP returns immediately.
const ringSource = `
redis.call('LPUSH', KEYS[1], '1')
redis.call('LTRIM', KEYS[1], 0, 0)
redis.call('EXPIRE', KEYS[1], ARGV[1])
return 1`

// ringScript sends EVALSHA and falls back to EVAL only on NOSCRIPT, matching
// every other Lua site in this repo (modules/oidc, modules/message,
// internal/cardactiondispatch, modules/incomingwebhook, modules/opanalytics).
//
// It also serves the stated reason for the script in the first place. Raw EVAL
// puts the whole body on the wire on every ring, on the hottest producer path —
// which works against the per-message cost the rewrite existed to reduce. The
// fallback costs a second round trip only on a cold Redis or after SCRIPT
// FLUSH; steady state is one EVALSHA.
var ringScript = rd.NewScript(ringSource)

// Ringer is the Redis surface Ring needs: go-redis's own scripting set, which
// is what *rd.Script.Run requires. octo-lib's *redis.Conn exposes none of it, so
// producers pass the instrumented raw client from RingClient.
//
// Declared here rather than using go-redis's unexported `scripter` so a test can
// substitute a fake, and so the dependency this package places on its callers is
// written down.
type Ringer interface {
	Eval(script string, keys []string, args ...interface{}) *rd.Cmd
	EvalSha(sha1 string, keys []string, args ...interface{}) *rd.Cmd
	ScriptExists(hashes ...string) *rd.BoolSliceCmd
	ScriptLoad(script string) *rd.StringCmd
}

// BellKey returns the doorbell list key for robotID. Callers MUST pass the
// robot identity resolved from the authenticated request context, never a
// value taken from a request body — the bell key shares the bot-ownership
// boundary of the event queue itself.
func BellKey(robotID string) string {
	return BellKeyPrefix + robotID
}

// Ring publishes a wake-up hint for robotID.
//
// It is deliberately **best-effort and non-fatal**: producers call it after a
// successful ZADD, and a failure here must never fail or roll back an enqueue
// that already succeeded. The cost of a dropped bell is bounded by the
// consumer's hold duration; the cost of failing an accepted enqueue is a lost
// event.
//
// The list is trimmed to a single element after every push. One token is all a
// waiter needs ("something changed, go re-read the queue"), and the constant
// length means an unattended bell cannot grow without bound when no consumer is
// long-polling.
//
// The error is returned rather than swallowed, and every producer now logs it
// at Warn. That matters more since the ring moved onto its own connection pool:
// there is a state where the ZADD succeeds through a warm shared-pool
// connection while the ring pool cannot get one at all, so bells vanish while
// nothing else looks wrong. A dropped bell is not free — it costs each waiter up
// to one chunk (5s), because the hold falls back to re-reading the authoritative
// queue at chunk boundaries rather than waiting out the whole `wait`. A counter
// still belongs to the card-ingress observability work (G1), which owns the
// metric namespace; one log line does not.
func Ring(r Ringer, robotID string) error {
	if r == nil || strings.TrimSpace(robotID) == "" {
		return nil
	}
	// A nil *rd.Client boxed in a non-nil Ringer passes the check above, so test
	// the concrete type too. RingClient cannot produce one today
	// (NewInstrumentedClient panics rather than returning nil), but the
	// interface-nil check alone would be false confidence, not a guard.
	if c, ok := r.(*rd.Client); ok && c == nil {
		return nil
	}
	return ringScript.Run(r, []string{BellKey(robotID)}, int64(BellTTL/time.Second)).Err()
}
