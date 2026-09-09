package ai_team

import "errors"

var (
	errForbidden           = errors.New("ai team permission denied")
	errInvalid             = errors.New("invalid ai team request")
	errNotFound            = errors.New("ai team resource not found")
	errIdempotencyConflict = errors.New("ai team idempotency conflict")
	errTeamCreateLimit     = errors.New("ai team daily create limit reached")
	errIMUnavailable       = errors.New("ai team IM unavailable")
)
