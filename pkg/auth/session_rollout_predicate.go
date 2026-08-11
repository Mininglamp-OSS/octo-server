package auth

// The rollout advance predicate.
//
// This replaces the two-observation, one-hour-apart evidence ritual. That
// ritual proved "no legacy remains" EMPIRICALLY — scan twice and hope the gap
// caught a straggler writer. A deductive proof was available all along:
//
//	no legacy remains  ⟸  no legacy writer can exist
//	                   ∧  every legacy record has a bounded deadline
//	                   ∧  that deadline has passed
//
// The floor already machine-enforces the first term for NEW processes; the hole
// was processes already running, which is exactly what the wall clock was
// standing in for. A writer registry answers it by query instead of by waiting,
// so the ritual — and its two hours — go away.
//
// The predicate is evaluated fresh at decision time rather than read from
// persisted evidence, which is why an empty keyspace is now the STRONGEST
// evidence rather than a rejected one, and why greenfield needs no special
// branch: v1=0 ∧ v2=0 holds trivially when there is nothing there.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
)

// The two convergence blockers are named constants because three places must
// agree on them: the predicate that emits them, the reconciler's backoff, and
// the CLI's watch loop. Two of those matched them with a hand-written substring.
const (
	convergenceBlockedPrefix    = "writer set stable for "
	convergenceUnobservedReason = "writer convergence has not been observed over time"
)

// RolloutAdvanceDecision is what the reconciler and `advance --force` both act
// on. BlockedBy entries are operator-facing and low cardinality.
type RolloutAdvanceDecision struct {
	Current   SessionMode `json:"current_floor"`
	Target    SessionMode `json:"target_floor"`
	Allowed   bool        `json:"allowed"`
	BlockedBy []string    `json:"blocked_by,omitempty"`
	MaxPerUID int         `json:"max_per_uid,omitempty"`
	// Scanned reports whether this evaluation ran a keyspace scan, so a caller
	// can back off rather than rescanning every cycle while waiting out a
	// deadline measured in days.
	Scanned bool `json:"scanned"`
	// Observation is the scan that justified the decision. It is carried here so
	// the advance snapshot records the counts it claims to audit — a snapshot
	// reading v1=0 v2=0 because nobody filled it in is indistinguishable from
	// one that actually looked.
	Observation *SessionObservation `json:"observation,omitempty"`
	// Options are operator-actionable next steps for the blockers above.
	Options []string `json:"options,omitempty"`
}

// RolloutAdvanceInput carries what the predicate cannot read for itself.
type RolloutAdvanceInput struct {
	Registry *WriterRegistry
	// ExpectWriters is the replica count the deployment intends to run. Required
	// for the first v3 floor and only that one, because it is the single
	// transition where a non-participating pre-#725 build could still be writing
	// v2 and the registry structurally cannot see it. Every later transition is
	// fully machine-gated.
	ExpectWriters int
	// MaxPerUID is required when crossing into a v3-writing floor.
	MaxPerUID int
	// Markers answers the one question a missing floor cannot answer about
	// itself: was one ever established here? Required whenever the floor reads
	// as absent, because "never created" and "created and lost" need opposite
	// reactions — see .octospec/tasks/.../boot-state-table.md rows C3/C4.
	Markers *RolloutMarkerStore
	// Convergence carries the caller's observation window across evaluations.
	// Required for the first v3 floor, where a count taken at one instant is not
	// enough — see WriterConvergence. A caller that cannot observe over time
	// (a one-shot command) supplies one and evaluates repeatedly.
	Convergence *WriterConvergence
	// ScanBatchSize and ScanInterval throttle the keyspace scan. A zero interval
	// is reserved for tests; production callers pass a positive one, because
	// this scan runs unattended and on the shared session pool.
	ScanBatchSize int64
	ScanInterval  time.Duration
}

// EvaluateRolloutAdvance decides whether the floor may move one phase.
func (s *RedisSessionStore) EvaluateRolloutAdvance(ctx context.Context, in RolloutAdvanceInput) (RolloutAdvanceDecision, error) {
	control, err := s.RolloutControl(ctx)
	if err != nil {
		return RolloutAdvanceDecision{}, err
	}

	decision := RolloutAdvanceDecision{Target: SessionModeV3Write, MaxPerUID: in.MaxPerUID}
	if control != nil {
		decision.Current = control.ModeFloor
		decision.Target = control.ModeFloor.next()
		if decision.MaxPerUID <= 0 {
			decision.MaxPerUID = control.MaxPerUID
		}
		if control.ModeFloor == SessionModeEnforce {
			// Terminal. Nothing to evaluate and nothing to scan — the
			// reconciler goes quiet from here.
			decision.BlockedBy = []string{"floor is already enforce"}
			return decision, nil
		}
	}

	if control == nil {
		// A floor that reads as absent is three states, not one: never created,
		// created and lost, or not created yet. Only the marker separates them,
		// and this path had no marker at all — so an in-flight loss (RDB
		// rollback, an accidental DEL) read as greenfield and the reconciler
		// re-created the ladder at v3-write OVER a stamped marker. Any replica
		// restarting in that window boots at v3-write, which accepts exactly the
		// persistent and over-max legacy bearers bounded had begun rejecting.
		//
		// Recovery, not advancement, is the answer to a lost floor: it restores
		// enforce in one write with the marker as its evidence. So refuse here
		// and let the poller's recovery path do it — one writer for that job.
		switch {
		case in.Markers == nil:
			decision.BlockedBy = append(decision.BlockedBy,
				"cannot determine whether a floor was ever initialised here")
		default:
			marker, markerErr := in.Markers.Load(ctx)
			switch {
			case markerErr != nil && !isMissingMarkerTable(markerErr):
				decision.BlockedBy = append(decision.BlockedBy,
					"initialisation marker is unreadable")
			case markerErr == nil && marker != nil:
				decision.BlockedBy = append(decision.BlockedBy,
					fmt.Sprintf("floor is missing but this deployment initialised one at %s; "+
						"it must be recovered, not re-created",
						marker.InitializedAt.UTC().Format(time.RFC3339)))
			}
		}
	}

	if decision.Target.writesV3() && (decision.MaxPerUID <= 0 || decision.MaxPerUID > sessionMaxPerUIDLimit) {
		decision.BlockedBy = append(decision.BlockedBy, "max_per_uid is not configured")
	}

	// Fleet convergence. Every live writer must already be running at the
	// current floor, or advancing would put the floor two phases ahead of a
	// replica that has not caught up.
	if in.Registry == nil {
		decision.BlockedBy = append(decision.BlockedBy, "writer registry unavailable")
	} else {
		live, liveErr := in.Registry.Live()
		if liveErr != nil {
			return RolloutAdvanceDecision{}, liveErr
		}
		switch {
		case len(live) == 0:
			// An empty registry is a FAILURE, not a pass. Note this is the
			// mirror of the token scan: there, emptiness proves absence and is
			// the strongest evidence; here we are proving presence and
			// convergence, so emptiness only means we cannot see.
			decision.BlockedBy = append(decision.BlockedBy, "no live writers registered")
		default:
			builds := map[string]struct{}{}
			behind := 0
			for _, entry := range live {
				builds[entry.Build] = struct{}{}
				if SessionMode(entry.AppliedState).rank() < decision.Current.rank() {
					behind++
				}
			}
			if len(builds) > 1 {
				decision.BlockedBy = append(decision.BlockedBy,
					fmt.Sprintf("%d distinct builds are live", len(builds)))
			}
			if behind > 0 {
				decision.BlockedBy = append(decision.BlockedBy,
					fmt.Sprintf("%d of %d writers have not applied floor %s", behind, len(live), decision.Current))
			}
		}
	}

	// bounded is scanned too. It was omitted, and because bounded rejects every
	// persistent and over-max legacy record at read time, a converged fleet
	// could be walked into a user-visible denial phase with no scan, no
	// migration and no cutoff — logging out exactly the population whose fate
	// is supposed to be the operator's decision.
	needScan := decision.Target == SessionModeEnforce ||
		decision.Target == SessionModeV3Write ||
		decision.Target == SessionModeBounded
	var observation SessionObservation
	if needScan {
		batch := in.ScanBatchSize
		if batch <= 0 {
			batch = 200
		}
		observation, err = s.ObserveRateLimited(ctx, batch, in.ScanInterval)
		if err != nil {
			return RolloutAdvanceDecision{}, err
		}
		decision.Scanned = true
		decision.Observation = &observation
		if !observation.Complete {
			decision.BlockedBy = append(decision.BlockedBy, "keyspace scan did not complete")
		}
	}

	if decision.Target == SessionModeV3Write {
		// Required unconditionally. It used to be skipped on an empty keyspace,
		// reasoning that nothing written means no writer exists — but empty only
		// means nothing is stored RIGHT NOW, which is also true after a TTL
		// sweep, a flush, or the Redis loss this change handles. An idle
		// pre-registry replica writes v2 on the next login and registers
		// nowhere, so neither the roster nor the build check can see it.
		var entries []WriterEntry
		if in.Registry != nil {
			if observed, liveErr := in.Registry.Live(); liveErr == nil {
				entries = observed
			}
		}
		live := len(entries)
		switch {
		case in.ExpectWriters <= 0:
			decision.BlockedBy = append(decision.BlockedBy,
				fmt.Sprintf("%s is required to establish the first v3 floor", sessionExpectWritersEnv))
		case live != in.ExpectWriters:
			decision.BlockedBy = append(decision.BlockedBy,
				fmt.Sprintf("expected %d writers, registry has %d", in.ExpectWriters, live))
		}

		// The count above is a snapshot, and a snapshot is satisfiable during a
		// maxSurge:1 rollout at the instant the new replicas are all up and the
		// last pre-registry one has not yet exited — the exact replica this gate
		// exists to exclude. Require the SET to have held still for a full lease
		// TTL, which (identities being per-incarnation) proves no membership
		// change occurred and therefore that the rollout has stopped moving.
		//
		// brief §2 specified this alongside the 5s/30s numbers; it is also the
		// stated mitigation for the check-then-write window in writableState.
		switch {
		case in.Convergence == nil:
			decision.BlockedBy = append(decision.BlockedBy,
				convergenceUnobservedReason)
		default:
			stable := in.Convergence.Observe(entries, s.now())
			if stable < writerLeaseTTL {
				decision.BlockedBy = append(decision.BlockedBy,
					fmt.Sprintf("%s%s, need %s", convergenceBlockedPrefix, stable.Truncate(time.Second), writerLeaseTTL))
			}
		}
	}

	if decision.Target == SessionModeBounded && (observation.Persistent != 0 || observation.OverMax != 0) {
		// Exactly the set bounded rejects, so the gate mirrors the reader.
		decision.BlockedBy = append(decision.BlockedBy,
			fmt.Sprintf("persistent=%d over_max=%d (need 0)", observation.Persistent, observation.OverMax))
		decision.Options = append(decision.Options,
			"wait: legacy records expire on their own",
			"migrate --finite-policy natural --cutoff <T>: converge them on an approved deadline")
	}

	if decision.Target == SessionModeEnforce {
		if observation.V1 != 0 || observation.V2 != 0 {
			decision.BlockedBy = append(decision.BlockedBy,
				fmt.Sprintf("v1=%d v2=%d (need 0)", observation.V1, observation.V2))
			decision.Options = append(decision.Options,
				"wait: active users are promoted on reuse; inactive ones expire at TokenExpire",
				"migrate --finite-policy cap --cutoff <T>: converge them, at the cost of an early re-login")
		}
		// decode_invalid is deliberately NOT a blocker. A record that fails
		// Decode was never a usable credential — the validator has always
		// rejected it — so letting it hold the floor was the evidence validator
		// being stricter than the security requirement. It is reported and
		// alarmed instead, because nothing clears it on its own.
		if observation.DecodeInvalid != 0 {
			metrics.SetSessionUndecodableRecords(observation.DecodeInvalid)
		}
	}
	if decision.Scanned {
		metrics.SetSessionUndecodableRecords(observation.DecodeInvalid)
	}

	sort.Strings(decision.BlockedBy)
	decision.Allowed = len(decision.BlockedBy) == 0
	return decision, nil
}

// BlockedSummary renders BlockedBy for a log line or status output.
func (d RolloutAdvanceDecision) BlockedSummary() string {
	if len(d.BlockedBy) == 0 {
		return ""
	}
	return strings.Join(d.BlockedBy, "; ")
}

// blockedOnlyOnConvergence reports whether the sole thing standing in the way is
// the passage of time. Those blockers resolve by re-asking; every other one
// describes the fleet or the keyspace and will not change because a caller asked
// twice, so waiting on them is just a slower refusal.
//
// Both the reconciler's backoff and the CLI's watch loop key off this, and they
// must agree: a divergence would mean the command an operator runs and the loop
// it is diagnosing disagree about whether waiting can help.
func blockedOnlyOnConvergence(decision RolloutAdvanceDecision) bool {
	if len(decision.BlockedBy) == 0 {
		return false
	}
	for _, reason := range decision.BlockedBy {
		if !strings.Contains(reason, convergenceBlockedPrefix) &&
			!strings.Contains(reason, convergenceUnobservedReason) {
			return false
		}
	}
	return true
}

// SplitUndeterminableBlockers separates blockers a one-shot caller cannot
// evaluate from ones it can. Only the convergence window qualifies: it asserts
// that a set held still across a lease TTL, and a single invocation has no
// window to have observed. Reporting it next to real obstacles made `status`
// contradict the reconciler it exists to diagnose.
func SplitUndeterminableBlockers(blockedBy []string) (undeterminable, remaining []string) {
	for _, reason := range blockedBy {
		if strings.Contains(reason, convergenceBlockedPrefix) ||
			strings.Contains(reason, convergenceUnobservedReason) {
			undeterminable = append(undeterminable, reason)
			continue
		}
		remaining = append(remaining, reason)
	}
	return undeterminable, remaining
}

// BlockedOnlyOnConvergence is the exported form, for callers outside this
// package that poll the predicate.
func (d RolloutAdvanceDecision) BlockedOnlyOnConvergence() bool { return blockedOnlyOnConvergence(d) }
