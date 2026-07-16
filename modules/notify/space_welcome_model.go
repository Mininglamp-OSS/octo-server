package notify

import "time"

// Space new-user welcome delivery ledger — task space-new-user-welcome-message.
//
// This file holds the shared constants (status machine, error_class, stage,
// tunables) for the onboarding welcome feature. The ledger table
// octo_space_welcome_delivery is the permanent, monotonically-growing dedupe
// source of truth; send semantics are at-most-once.

const spaceWelcomeTable = "octo_space_welcome_delivery"

// Ledger row status machine. Terminal states: sent / failed / skipped / unknown.
const (
	swStatusPending     = 0 // claimable by worker
	swStatusClaimed     = 1 // worker locked the row; IM call not yet started; sweep => pending
	swStatusDispatching = 2 // worker began the IM call; sweep => unknown (transport-ambiguous)
	swStatusSent        = 3 // terminal success
	swStatusFailed      = 4 // terminal (pre-IM retry budget exhausted)
	swStatusSkipped     = 5 // terminal (recipient ineligible at pre-send check)
	swStatusUnknown     = 6 // terminal (transport-ambiguous; operator handles)
)

// error_class enumeration (structured log field + ledger column).
const (
	swErrBotNotReady   = "bot_not_ready"
	swErrMemberLeft    = "member_left"
	swErrHumanFilter   = "human_filter"
	swErrOrphanMember  = "orphan_member"
	swErrConfigRead    = "config_read"
	swErrIMTimeout     = "im_timeout"
	swErrIMBadResponse = "im_bad_response"
	swErrSentPersist   = "sent_persist"
	swErrClaimExpired  = "claim_expired"
)

// stage enumeration (structured log field).
const (
	swStageEnqueue  = "enqueue"
	swStageSweep    = "sweep"
	swStageClaim    = "claim"
	swStagePrecheck = "precheck"
	swStageDispatch = "dispatch"
	swStagePersist  = "persist"
)

const (
	// swDBCallTimeout bounds every DB call the worker issues so precheck + CAS
	// overhead stays well under the 30s claim lease rather than dangling.
	swDBCallTimeout = 3 * time.Second
	// swClaimLease is how long a claimed/dispatching row is leased to one owner
	// before a peer's sweep may reclaim it. 15s IM timeout < 30s lease, so a hung
	// request can never outlive the lease.
	swClaimLease = 30 * time.Second
	// swReconcileInterval is the reconciler cadence (compile-time constant).
	swReconcileInterval = 60 * time.Second
	// swReconcileCap bounds rows enqueued per reconcile cycle.
	swReconcileCap = 500
	// swWorkerWakeCap is the per-wake safety valve: after this many successful
	// claims in one wake the worker falls through to the idle sleep so a single
	// goroutine cannot monopolise the shared DB session.
	swWorkerWakeCap = 20
	// swIdleSleep is the steady-state poll interval when the queue is empty.
	swIdleSleep = 5 * time.Second
	// swHTTPTimeout is the notify-local sender's authoritative timeout — the
	// socket is actually closed on expiry (unlike octo-lib's helper).
	swHTTPTimeout = 15 * time.Second
)

// swBackoff is the pre-IM retry backoff for attempts 1/2/3. After the 4th
// consecutive pre-IM failure the row moves to failed. attempts counts ONLY
// consecutive pre-IM failures.
var swBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 120 * time.Second}

// swMaxPreIMAttempts is the number of pre-IM failures tolerated before the row
// becomes terminal-failed (the 4th failure fails it). Kept in sync with the
// length of swBackoff.
const swMaxPreIMAttempts = 3

// spaceWelcomeRow is the minimal claimed-row projection the worker needs.
type spaceWelcomeRow struct {
	ID       int64  `db:"id"`
	SpaceID  string `db:"space_id"`
	UID      string `db:"uid"`
	Attempts int    `db:"attempts"`
}
