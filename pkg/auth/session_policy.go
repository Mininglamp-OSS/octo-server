package auth

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	// sessionModeEnv and sessionMaxPerUIDEnv are DEPRECATED. The rollout mode is
	// derived from the persisted floor; the cap lives in the floor record. Both
	// are still read for one release so an upgrade from #725 cannot silently
	// loosen a deployment that is mid-canary (see ResolveRolloutBoot).
	sessionModeEnv      = "OCTO_AUTH_SESSION_MODE"
	sessionMaxPerUIDEnv = "OCTO_AUTH_SESSION_MAX_PER_UID"
	// sessionRequiredFloorEnv is REMOVED. The floor is its own authority; having
	// the environment restate it was the second half of the pairing that made a
	// lost Redis key fatal.
	sessionRequiredFloorEnv = "OCTO_AUTH_SESSION_REQUIRED_FLOOR"

	// sessionCanaryAheadEnv is the only remaining mode knob: run this replica one
	// phase above the floor. It replaces "deploy a higher MODE to a subset",
	// which is the same thing minus the ordering trap.
	sessionCanaryAheadEnv = "OCTO_AUTH_SESSION_CANARY_AHEAD"
	// sessionExpectWritersEnv is the replica count the deployment intends to run.
	// It is information the deployer already has and the process does not, not a
	// judgement call. Only consulted when first establishing a v3 floor, and
	// removable afterwards.
	sessionExpectWritersEnv = "OCTO_AUTH_SESSION_EXPECT_WRITERS"
	// sessionAutoAdvanceEnv is the deployment-side initial value for the
	// reconciler. The runtime pause flag wins over it, and the more conservative
	// of the two applies.
	sessionAutoAdvanceEnv = "OCTO_AUTH_SESSION_AUTO_ADVANCE"

	sessionMaxPerUIDLimit = 10_000
)

// SessionMode is the deployment phase for user HTTP sessions. Its zero value is
// deliberately invalid.
type SessionMode string

const (
	SessionModeExpand  SessionMode = "expand"
	SessionModeV3Write SessionMode = "v3-write"
	SessionModeRevoke  SessionMode = "revoke"
	SessionModeBounded SessionMode = "bounded"
	SessionModeEnforce SessionMode = "enforce"
)

type sessionPolicy struct {
	legacyMode      SessionMode
	legacyMaxPerUID int
	canaryAhead     bool
	expectWriters   int
	autoAdvance     bool
	// warnings are operator-facing configuration notes. They are logged, never
	// fatal: taking a deployment down over a stale environment variable is the
	// failure class this change exists to remove.
	warnings []string
}

func sessionPolicyFromEnv() sessionPolicy {
	policy := sessionPolicy{}

	if raw, ok := os.LookupEnv(sessionModeEnv); ok {
		value := strings.TrimSpace(raw)
		mode := SessionMode(value)
		switch {
		case mode.valid():
			policy.legacyMode = mode
			policy.warnings = append(policy.warnings, fmt.Sprintf(
				"%s is deprecated; the rollout mode is derived from the persisted floor. "+
					"It is honoured for this release so an upgrade cannot loosen a mid-canary deployment. "+
					"Remove it once the floor has caught up, or use %s=1 for a canary replica.",
				sessionModeEnv, sessionCanaryAheadEnv))
		default:
			policy.warnings = append(policy.warnings, fmt.Sprintf(
				"ignoring unparseable %s=%q; the rollout mode is derived from the persisted floor",
				sessionModeEnv, value))
		}
	}

	if raw, ok := os.LookupEnv(sessionMaxPerUIDEnv); ok {
		value := strings.TrimSpace(raw)
		parsed, err := strconv.ParseInt(value, 10, 32)
		switch {
		case err == nil && parsed > 0 && parsed <= sessionMaxPerUIDLimit:
			policy.legacyMaxPerUID = int(parsed)
			policy.warnings = append(policy.warnings, fmt.Sprintf(
				"%s is deprecated; the cap is carried in the rollout floor record. "+
					"It is copied across once and can then be removed.", sessionMaxPerUIDEnv))
		default:
			policy.warnings = append(policy.warnings, fmt.Sprintf(
				"ignoring out-of-range %s=%q (want 1..%d)", sessionMaxPerUIDEnv, value, sessionMaxPerUIDLimit))
		}
	}

	if _, ok := os.LookupEnv(sessionRequiredFloorEnv); ok {
		policy.warnings = append(policy.warnings, fmt.Sprintf(
			"%s has no effect and can be deleted; the persisted floor is its own authority",
			sessionRequiredFloorEnv))
	}

	policy.canaryAhead = envFlag(sessionCanaryAheadEnv, false)
	policy.autoAdvance = envFlag(sessionAutoAdvanceEnv, false)

	if raw, ok := os.LookupEnv(sessionExpectWritersEnv); ok {
		value := strings.TrimSpace(raw)
		parsed, err := strconv.ParseInt(value, 10, 32)
		if err != nil || parsed <= 0 {
			policy.warnings = append(policy.warnings, fmt.Sprintf(
				"ignoring unparseable %s=%q; the first v3 floor will stay blocked",
				sessionExpectWritersEnv, value))
		} else {
			policy.expectWriters = int(parsed)
		}
	}
	return policy
}

func envFlag(name string, fallback bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off", "":
		return false
	default:
		return fallback
	}
}

func (m SessionMode) valid() bool {
	switch m {
	case SessionModeExpand, SessionModeV3Write, SessionModeRevoke, SessionModeBounded, SessionModeEnforce:
		return true
	default:
		return false
	}
}

func (m SessionMode) writesV3() bool {
	return m.rank() >= SessionModeV3Write.rank()
}

// WritesV3 reports whether the rollout phase permits creating or promoting
// user HTTP sessions with the v3 envelope.
func (m SessionMode) WritesV3() bool {
	return m.writesV3()
}

// RevokesSessions reports whether durable security events may rotate
// generations and create legacy deny markers in this rollout phase.
func (m SessionMode) RevokesSessions() bool {
	return m.rank() >= SessionModeRevoke.rank()
}

func (m SessionMode) checksLegacyDeny() bool {
	return m.rank() >= SessionModeV3Write.rank() && m != SessionModeEnforce
}

func (m SessionMode) rank() int {
	switch m {
	case SessionModeExpand:
		return 1
	case SessionModeV3Write:
		return 2
	case SessionModeRevoke:
		return 3
	case SessionModeBounded:
		return 4
	case SessionModeEnforce:
		return 5
	default:
		return 0
	}
}

// next returns the phase one step above m, or m itself at the top.
func (m SessionMode) next() SessionMode {
	switch m {
	case SessionModeExpand:
		return SessionModeV3Write
	case SessionModeV3Write:
		return SessionModeRevoke
	case SessionModeRevoke:
		return SessionModeBounded
	case SessionModeBounded:
		return SessionModeEnforce
	default:
		return SessionModeEnforce
	}
}
