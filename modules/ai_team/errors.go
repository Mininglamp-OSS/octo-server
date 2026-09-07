package ai_team

import "errors"

var (
	errForbidden           = errors.New("ai team permission denied")
	errNotFound            = errors.New("ai team resource not found")
	errIdempotencyConflict = errors.New("ai team idempotency conflict")
	errIMUnavailable       = errors.New("ai team IM unavailable")
)
