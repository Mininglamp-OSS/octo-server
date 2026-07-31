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
)

// BellKeyPrefix namespaces the per-bot doorbell list. Kept distinct from
// `robotEvent:` so the authoritative queue and the hint can never collide.
const BellKeyPrefix = "robotEventBell:"

// BellTTL bounds how long an unread doorbell token survives. It only has to
// outlive one long-poll hold (30s ceiling) — a bell that expires while an event
// still sits in the queue costs nothing, because consumers read the sorted set
// before they ever wait. Five minutes is slack, not a requirement.
const BellTTL = 5 * time.Minute

// Ringer is the narrow Redis surface Ring needs. octo-lib's *redis.Conn
// satisfies it, so producers ring on the ordinary shared pool: LPUSH/LTRIM/
// EXPIRE are short non-blocking commands. Only the *waiting* side needs an
// isolated pool, because BLPOP holds a connection for its whole duration.
type Ringer interface {
	LPUSH(key string, values ...interface{}) (int64, error)
	Ltrim(key string, start, stop int64) (string, error)
	Expire(key string, expiration time.Duration) error
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
// The error is returned so a caller *may* act on it, but the current producers
// discard it — matching the local convention in those helpers, where the
// best-effort TTL refresh beside them is swallowed the same way. Be aware of
// what that buys and costs: a persistently failing bell degrades every hold to
// "wait out the full timeout" **silently**, with no signal anywhere. Closing
// that blind spot belongs to the card-ingress observability work (G1), which
// owns the metric namespace; adding an ad-hoc logger here would put the first
// counter in the wrong place.
func Ring(r Ringer, robotID string) error {
	if r == nil || strings.TrimSpace(robotID) == "" {
		return nil
	}
	key := BellKey(robotID)
	if _, err := r.LPUSH(key, "1"); err != nil {
		return err
	}
	// Keep index 0 only. Runs after the push so the list is never observed
	// empty by a waiter that is about to block.
	if _, err := r.Ltrim(key, 0, 0); err != nil {
		return err
	}
	return r.Expire(key, BellTTL)
}
