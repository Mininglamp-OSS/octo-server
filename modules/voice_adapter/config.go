package voice_adapter

import (
	"os"
	"strconv"
	"time"
)

type AdapterConfig struct {
	SpeechServiceURL string
	SpeechAPIKey     string
	SpeechTimeout    time.Duration
}

func NewAdapterConfigFromEnv() *AdapterConfig {
	timeoutSec := 50
	if v := os.Getenv("SPEECH_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeoutSec = n
		}
	}

	return &AdapterConfig{
		SpeechServiceURL: os.Getenv("SPEECH_SERVICE_URL"),
		SpeechAPIKey:     os.Getenv("SPEECH_API_KEY"),
		SpeechTimeout:    time.Duration(timeoutSec) * time.Second,
	}
}
