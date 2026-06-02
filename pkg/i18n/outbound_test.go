package i18n

import (
	"context"
	"testing"
)

func TestOutboundLanguage(t *testing.T) {
	t.Run("background ctx falls back to env default", func(t *testing.T) {
		t.Setenv(EnvDefaultLanguage, "en-US")
		if got := OutboundLanguage(context.Background()); got != "en-US" {
			t.Fatalf("OutboundLanguage = %q, want en-US", got)
		}
	})

	t.Run("empty env uses DefaultLanguage", func(t *testing.T) {
		t.Setenv(EnvDefaultLanguage, "")
		if got := OutboundLanguage(context.Background()); got != DefaultLanguage {
			t.Fatalf("OutboundLanguage = %q, want %q", got, DefaultLanguage)
		}
	})

	t.Run("invalid env falls back to DefaultLanguage", func(t *testing.T) {
		t.Setenv(EnvDefaultLanguage, "not-a-lang")
		if got := OutboundLanguage(context.Background()); got != DefaultLanguage {
			t.Fatalf("OutboundLanguage = %q, want %q", got, DefaultLanguage)
		}
	})

	t.Run("ctx decision wins over env default", func(t *testing.T) {
		t.Setenv(EnvDefaultLanguage, "en-US")
		ctx := WithLanguage(context.Background(), LanguageDecision{
			Language: "zh-CN",
			Source:   LanguageSourceQuery,
		})
		if got := OutboundLanguage(ctx); got != "zh-CN" {
			t.Fatalf("OutboundLanguage = %q, want zh-CN (ctx should win)", got)
		}
	})
}
