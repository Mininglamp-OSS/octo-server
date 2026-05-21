package voice_adapter

import (
	"os"
	"strconv"
	"time"
)

const (
	EnvSpeechServiceURL  = "SPEECH_SERVICE_URL"
	EnvSpeechAPIKey      = "SPEECH_API_KEY"
	EnvSpeechTimeout     = "SPEECH_TIMEOUT"
	EnvSpeechMaxBodySize = "SPEECH_MAX_BODY_SIZE"
	DefaultTimeoutSec    = 50
)

type AdapterConfig struct {
	SpeechServiceURL string
	SpeechAPIKey     string
	SpeechTimeout    time.Duration
	MaxBodySize      int64
}

func NewAdapterConfigFromEnv() *AdapterConfig {
	timeoutSec := DefaultTimeoutSec
	if v := os.Getenv(EnvSpeechTimeout); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeoutSec = n
		}
	}

	maxBodySize := int64(5 << 20)
	if v := os.Getenv(EnvSpeechMaxBodySize); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			maxBodySize = n
		}
	}

	return &AdapterConfig{
		SpeechServiceURL: os.Getenv(EnvSpeechServiceURL),
		SpeechAPIKey:     os.Getenv(EnvSpeechAPIKey),
		SpeechTimeout:    time.Duration(timeoutSec) * time.Second,
		MaxBodySize:      maxBodySize,
	}
}
