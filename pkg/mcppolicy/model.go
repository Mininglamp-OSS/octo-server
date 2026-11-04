// Package mcppolicy stores explicit Bot overrides and the mandatory stage
// default. Authentication belongs to its callers; policy never grants identity.
package mcppolicy

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Mode string

const (
	Compatible Mode = "compatible"
	Strict     Mode = "strict"
)

var (
	ErrInvalid     = errors.New("mcp: invalid policy request")
	ErrUnavailable = errors.New("mcp: policy unavailable")
	ErrConflict    = errors.New("mcp: policy generation conflict")
)

func (m Mode) Valid() bool { return m == Compatible || m == Strict }

func ValidUID(uid string) bool {
	if len(uid) < 1 || len(uid) > 40 || !utf8.ValidString(uid) || strings.TrimSpace(uid) != uid {
		return false
	}
	for _, r := range uid {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ParseGeneration accepts only the lossless wire representation, not JSON
// numbers, signs, whitespace, zero, or leading zeroes.
func ParseGeneration(s string) (uint64, error) {
	if s == "" || s[0] < '1' || s[0] > '9' {
		return 0, ErrInvalid
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrInvalid
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, ErrInvalid
	}
	return n, nil
}

type Policy struct {
	Version      int     `json:"version"`
	ContextMode  Mode    `json:"context_mode"`
	Override     *Mode   `json:"override"`
	StageDefault Mode    `json:"stage_default"`
	Source       string  `json:"source"`
	Generation   string  `json:"generation"`
	BotRevision  *string `json:"bot_revision"`
}

type BotPolicy struct {
	BotUID string `json:"bot_uid"`
	Policy
}

type DefaultPolicy struct {
	Version     int    `json:"version"`
	ContextMode Mode   `json:"context_mode"`
	Generation  string `json:"generation"`
}
