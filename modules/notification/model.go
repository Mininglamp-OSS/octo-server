package notification

import "time"

const notificationPauseCMD = "user.notification_pause.changed"

type pauseRecord struct {
	UID         string     `db:"uid"`
	PausedUntil *time.Time `db:"paused_until"`
	Revision    uint64     `db:"revision"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

type pauseResponse struct {
	Paused      bool       `json:"paused"`
	PausedUntil *time.Time `json:"paused_until"`
	Revision    uint64     `json:"revision"`
	ServerTime  time.Time  `json:"server_time"`
}

type updatePauseRequest struct {
	PausedUntil time.Time `json:"paused_until"`
}

type errorResponse struct {
	Code string `json:"code"`
}
