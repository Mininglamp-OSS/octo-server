package main

// `app cutover msgextra` — the #627 operator surface for the transactional
// message_extra version allocator, moved here from tools/msgextra-version so
// it ships inside the image.
//
//	preflight   read-only: current allocator state and the safe cutover floor
//	            (max already-issued version across MySQL and cached Redis sync
//	            cursors).
//	activate    flip legacy→transactional under the drain barrier; needs -yes.
//	            Uses -floor if given, else the preflight recommendation.
//	            Refuses a floor below the observed max or above MaxCutoverFloor.
//	status      state row + expected-mode guard only; no evidence scan.
//
// There is no "deactivate" action: rolling back to legacy cannot be done safely
// online (octo-lib's in-memory GenSeq HiLo cache would resume below the
// transactional high-water), so rollback is a documented maintenance-window
// coordinated procedure — see docs/msgextra-cutover-runbook.md §6.
//
// Activation is a runtime state-row flip, NOT a code deploy: it takes effect
// immediately across all replicas via the DB-authoritative state row. Run
// preflight first, verify the numbers, then activate.

import (
	"errors"
	"fmt"
	"io"

	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
)

func msgextraCutoverDomain() *cutoverDomain {
	return &cutoverDomain{
		name:       "msgextra",
		summary:    "#627 message_extra version allocator (legacy GenSeq → transactional per-channel sequence)",
		stateTable: msgextraseq.StateTable,
		preflight:  msgextraCutoverPreflight,
		activate:   msgextraCutoverActivate,
		status:     msgextraCutoverStatus,
	}
}

func msgextraCutoverPreflight(rt *cutoverRuntime, out io.Writer) error {
	// The floor evidence includes a scan of the messageExtraVersion:* keyspace,
	// so the Redis this run resolved is part of what the operator is judging.
	fprintRedisEndpoint(rt.cfg)
	res, err := msgextraseq.New(rt.ctx).Preflight()
	if err != nil {
		return err
	}
	fprintMsgextraPreflight(out, res)
	return nil
}

func msgextraCutoverActivate(rt *cutoverRuntime, out io.Writer) error {
	fprintRedisEndpoint(rt.cfg)
	store := msgextraseq.New(rt.ctx)
	// Always show the preflight first so the operator sees what they are acting on.
	res, err := store.Preflight()
	if err != nil {
		return err
	}
	fprintMsgextraPreflight(out, res)
	if res.CurrentMode == msgextraseq.ModeTransactional {
		fmt.Fprintln(out, "\nalready transactional — nothing to do")
		return nil
	}
	floor := rt.floor
	if floor == 0 {
		floor = res.RecommendedFloor
		fmt.Fprintf(out, "\n-floor not set: using preflight recommendation %d\n", floor)
	}
	if floor < res.RecommendedFloor {
		return fmt.Errorf("refusing: -floor=%d is below the recommended floor %d (would risk reissuing an existing version)", floor, res.RecommendedFloor)
	}
	if floor > msgextraseq.MaxCutoverFloor {
		return fmt.Errorf("refusing: -floor=%d exceeds the maximum %d (would make every reservation fail ErrOverflow)", floor, msgextraseq.MaxCutoverFloor)
	}
	if !rt.yes {
		return fmt.Errorf("activate is a state change across all replicas — re-run with -yes to confirm (floor=%d)", floor)
	}
	flipped, err := store.Activate(rt.deadline, floor)
	// flipped first: a post-commit connection-cleanup failure comes back as
	// (true, err), and reporting "activation failed" for a committed flip would
	// send the operator down the wrong branch of the runbook.
	if flipped {
		fmt.Fprintf(out, "\nACTIVATED: allocator is now transactional (cutover_floor=%d). Verify versions are monotonic and rerun preflight to confirm mode=transactional.\n", floor)
		if err != nil {
			return fmt.Errorf("the flip COMMITTED, but releasing the database connection failed: %w", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\nno change — allocator was already transactional")
	return nil
}

func msgextraCutoverStatus(rt *cutoverRuntime, out io.Writer) error {
	st, err := msgextraseq.New(rt.ctx).CurrentState(rt.deadline)
	switch {
	case errors.Is(err, msgextraseq.ErrStateRowMissing):
		// Report and keep going rather than exiting here. Missing row + armed
		// guard is precisely the combination that fails every message_extra
		// write closed, so the guard line below is the one an operator most
		// needs in this state.
		fmt.Fprintln(out, "state: MISSING — the migration has not run. Readers treat this as legacy.")
	case err != nil:
		return err
	default:
		fmt.Fprintf(out, "state: mode=%s epoch=%d cutover_floor=%d\n", msgextraModeName(st.Mode), st.Epoch, st.CutoverFloor)
	}
	fprintCutoverGuard(out, msgextraseq.ExpectedModeEnv, msgextraseq.ExpectedModeSpellings(), msgextraModeName)
	return nil
}

func fprintMsgextraPreflight(out io.Writer, res msgextraseq.PreflightResult) {
	fmt.Fprintf(out, "current: mode=%s epoch=%d cutover_floor=%d\n", msgextraModeName(res.CurrentMode), res.CurrentEpoch, res.CurrentFloor)
	fmt.Fprintf(out, "evidence: max(message_extra.version)=%d max(legacy seq boundary)=%d max(Redis cursor)=%d\n", res.MaxMessageExtraVersion, res.MaxLegacySeqBoundary, res.MaxRedisCursor)
	fmt.Fprintf(out, "redis scan visits: keys=%d fields=%d invalid_or_poisoned=%d (identifiers omitted)\n", res.RedisCursorKeyCount, res.RedisCursorFieldCount, res.InvalidRedisCursorFieldCount)
	fmt.Fprintf(out, "recommended cutover_floor=%d\n", res.RecommendedFloor)
}

// msgextraModeName renders a mode with the domain's own spelling table, so the
// name shown here and the guard values the allocator accepts have one source.
func msgextraModeName(mode int) string {
	return cutoverModeName(msgextraseq.ExpectedModeSpellings(), mode)
}
