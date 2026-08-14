package notification

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
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
	*r = updatePauseRequest{}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok, err := pauseField(fields, "duration"); err != nil {
		return err
	} else if ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("duration must be a string")
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		r.Duration = &value
	}
	if raw, ok, err := pauseField(fields, "mode"); err != nil {
		return err
	} else if ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("mode must be a string")
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		r.Mode = &value
	}
	if raw, ok, err := pauseField(fields, "paused_until"); err != nil {
		return err
	} else if ok {
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

func pauseField(fields map[string]json.RawMessage, name string) (json.RawMessage, bool, error) {
	var value json.RawMessage
	found := 0
	for key, candidate := range fields {
		if strings.EqualFold(key, name) {
			value = candidate
			found++
		}
	}
	if found > 1 {
		return nil, false, fmt.Errorf("duplicate %s field", name)
	}
	return value, found == 1, nil
}

func pauseModeMatches(value *string, want string) bool {
	return value != nil && strings.EqualFold(strings.TrimSpace(*value), want)
}
