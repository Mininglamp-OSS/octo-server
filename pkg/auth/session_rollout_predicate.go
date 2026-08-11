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
		live := 0
		if in.Registry != nil {
			if entries, liveErr := in.Registry.Live(); liveErr == nil {
				live = len(entries)
			}
		}
		switch {
		case in.ExpectWriters <= 0:
			decision.BlockedBy = append(decision.BlockedBy,
				fmt.Sprintf("%s is required to establish the first v3 floor", sessionExpectWritersEnv))
		case live != in.ExpectWriters:
			decision.BlockedBy = append(decision.BlockedBy,
				fmt.Sprintf("expected %d writers, registry has %d", in.ExpectWriters, live))
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
