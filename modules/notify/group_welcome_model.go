package notify

import "time"

// Group new-member welcome delivery ledger — task group-welcome-message.
//
// The group welcome mirrors the Space welcome delivery engine but posts a PUBLIC
// message into the group channel (channel_type=GROUP) instead of a personal DM,
// and has no platform-global fallback. It reuses the shared status machine /
// error_class / stage / tunable constants and the notify-local HTTP sender from
// the space_welcome_* files (same package); only the ledger table, the row
// projection, and the container-scoped SQL differ.

const groupWelcomeTable = "octo_group_welcome_delivery"

// Coalescing tunables — a burst of joins (a batch invite arrives as ONE
// GroupMemberAdd event → many pending rows within milliseconds) is delivered as a
// SINGLE public post naming everyone, instead of one post per joiner. This is the
// group welcome's answer to bulk-invite spam; the Space welcome (personal DMs) has
// no such concern and shares none of this.
const (
	// groupWelcomeCoalesceWindow holds a freshly-enqueued pending row back from the
	// worker briefly so co-arriving joins collect before the first claim. New rows
	// get next_retry_at = enqueue + window; the worker then batch-claims everything
	// due for the group at once. A few seconds is invisible for a welcome and makes
	// the "one event, N members" burst reliably coalesce even if the worker happens
	// to wake mid-insert.
	groupWelcomeCoalesceWindow = 3 * time.Second
	// groupWelcomeCoalesceMax bounds ONE coalesced post: at most this many ledger
	// rows are claimed + delivered together. A larger burst spills into later wakes
	// (one post per group per wake) rather than building an unbounded message /
	// transaction — which also naturally rate-limits a single group's posts.
	groupWelcomeCoalesceMax = 50
	// groupWelcomeCoalesceNames caps how many joiner names are listed verbatim in
	// the {member} substitution; beyond it the tail collapses to "…、nameK 等 N 人"
	// so even a max-size batch stays one short, readable line.
	groupWelcomeCoalesceNames = 10
)

// groupWelcomeRow is the minimal claimed-row projection the worker needs.
type groupWelcomeRow struct {
	ID       int64  `db:"id"`
	GroupNo  string `db:"group_no"`
	UID      string `db:"uid"`
	Attempts int    `db:"attempts"`
}
