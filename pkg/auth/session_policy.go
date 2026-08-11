package auth

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	sessionModeEnv          = "OCTO_AUTH_SESSION_MODE"
	sessionMaxPerUIDEnv     = "OCTO_AUTH_SESSION_MAX_PER_UID"
	sessionRequiredFloorEnv = "OCTO_AUTH_SESSION_REQUIRED_FLOOR"

	sessionMaxPerUIDLimit = 10_000
)

// SessionMode is the monotonic deployment phase for user HTTP sessions.
// Its zero value is deliberately invalid; runtime configuration defaults to
// expand only when the environment variable is absent.
type SessionMode string

const (
	SessionModeExpand  SessionMode = "expand"
	SessionModeV3Write SessionMode = "v3-write"
	SessionModeRevoke  SessionMode = "revoke"
	SessionModeBounded SessionMode = "bounded"
	SessionModeEnforce SessionMode = "enforce"
)

type sessionPolicy struct {
	mode          SessionMode
	maxPerUID     int
	requiredFloor SessionMode
}

func sessionPolicyFromEnv() (sessionPolicy, error) {
	mode := SessionModeExpand
	if raw, ok := os.LookupEnv(sessionModeEnv); ok {
		value := strings.TrimSpace(raw)
		mode = SessionMode(value)
		if !mode.valid() {
			return sessionPolicy{}, fmt.Errorf("invalid %s %q", sessionModeEnv, value)
		}
	}

	maxPerUID := 0
	if raw, ok := os.LookupEnv(sessionMaxPerUIDEnv); ok {
		value := strings.TrimSpace(raw)
		parsed, err := strconv.ParseInt(value, 10, 32)
		if err != nil || parsed <= 0 {
			return sessionPolicy{}, fmt.Errorf("%s must be a positive integer", sessionMaxPerUIDEnv)
		}
		if parsed > sessionMaxPerUIDLimit {
			return sessionPolicy{}, fmt.Errorf("%s must not exceed %d", sessionMaxPerUIDEnv, sessionMaxPerUIDLimit)
		}
		maxPerUID = int(parsed)
	}
	if mode.writesV3() && maxPerUID == 0 {
		return sessionPolicy{}, fmt.Errorf("%s must be explicitly configured when %s=%s", sessionMaxPerUIDEnv, sessionModeEnv, mode)
	}
	var requiredFloor SessionMode
	if raw, ok := os.LookupEnv(sessionRequiredFloorEnv); ok {
		value := strings.TrimSpace(raw)
		requiredFloor = SessionMode(value)
		if !requiredFloor.valid() {
			return sessionPolicy{}, fmt.Errorf("invalid %s %q", sessionRequiredFloorEnv, value)
		}
		if mode.rank() < requiredFloor.rank() {
			return sessionPolicy{}, fmt.Errorf("%s=%s is below %s=%s", sessionModeEnv, mode, sessionRequiredFloorEnv, requiredFloor)
		}
	}
	return sessionPolicy{mode: mode, maxPerUID: maxPerUID, requiredFloor: requiredFloor}, nil
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
