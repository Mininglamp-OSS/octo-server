package notification

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

const notificationPauseCMD = "user.notification_pause.changed"

const (
	pauseModeTimed  = "timed"
	pauseModeManual = "manual"
)

type pauseRecord struct {
	UID         string     `db:"uid"`
	Mode        *string    `db:"mode"`
	PausedUntil *time.Time `db:"paused_until"`
	Revision    uint64     `db:"revision"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

type pauseResponse struct {
	Paused      bool       `json:"paused"`
	Mode        *string    `json:"mode"`
	PausedUntil *time.Time `json:"paused_until"`
	Revision    uint64     `json:"revision"`
	ServerTime  time.Time  `json:"server_time"`
}

type updatePauseRequest struct {
	Duration       *string
	Mode           *string
	PausedUntil    *time.Time
	hasPausedUntil bool
}

func (r *updatePauseRequest) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for name := range fields {
		if name != "duration" && name != "mode" && name != "paused_until" {
			return fmt.Errorf("unknown notification pause field %q", name)
		}
	}
	if raw, ok := fields["duration"]; ok {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		r.Duration = &value
	}
	if raw, ok := fields["mode"]; ok {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		r.Mode = &value
	}
	if raw, ok := fields["paused_until"]; ok {
		r.hasPausedUntil = true
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("paused_until must be an RFC3339 timestamp")
		}
		var value time.Time
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		r.PausedUntil = &value
	}
	return nil
}
