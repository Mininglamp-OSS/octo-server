package auth

// What remains here after the redesign: the migration completion record, kept
// as an AUDIT artefact rather than a gate, and the scope fingerprint. The
// observation-evidence machinery (two spaced scans, scan IDs, a persisted
// history) is gone — the predicate in session_rollout_predicate.go evaluates
// the keyspace at decision time instead, which is both stronger and free of
// the wall clock.

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	rd "github.com/go-redis/redis"
)

type migrationCompletionEvidence struct {
	CampaignID      string `json:"campaign_id"`
	CompletedAtMS   int64  `json:"completed_at_unix_ms"`
	RedisInstanceID string `json:"redis_instance_id"`
}

func (s *RedisSessionStore) recordMigrationCompletion(campaignID, redisInstanceID string, result LegacyMigrationResult) error {
	if !result.Complete || result.LockLost || result.CampaignID != campaignID || strings.TrimSpace(redisInstanceID) == "" {
		return errors.New("auth: cannot record incomplete migration evidence")
	}
	evidence := migrationCompletionEvidence{
		CampaignID:      campaignID,
		CompletedAtMS:   s.now().UTC().UnixMilli(),
		RedisInstanceID: redisInstanceID,
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("auth: encode migration completion evidence: %w", err)
	}
	if err := s.client.Set(s.latestMigrationCompletionKey(), string(encoded), 0).Err(); err != nil {
		return fmt.Errorf("auth: record migration completion evidence: %w", err)
	}
	return nil
}

// loadMigrationCompletionEvidence reads the audit record written by a completed
// apply. It no longer gates a floor advance — the predicate scans live — but it
// stays readable so `status` can show which campaign last finished and on which
// Redis instance.
func (s *RedisSessionStore) loadMigrationCompletionEvidence() (migrationCompletionEvidence, error) {
	var evidence migrationCompletionEvidence
	if err := s.loadPersistentEvidence(s.latestMigrationCompletionKey(), &evidence); err != nil {
		return migrationCompletionEvidence{}, fmt.Errorf("auth: migration completion evidence: %w", err)
	}
	if evidence.CampaignID == "" || evidence.CompletedAtMS <= 0 || evidence.RedisInstanceID == "" {
		return migrationCompletionEvidence{}, errors.New("auth: migration completion evidence is invalid")
	}
	return evidence, nil
}

// rolloutObservationScopeFingerprint identifies the scope an observation
// covers. It now includes the Redis instance: without it, two genuinely
// different endpoints reached with one config produced byte-identical
// fingerprints, which is how a misplaced config key once sent a tool to the
// wrong Redis without a word of complaint. Instance identity is best effort —
// a proxy that hides INFO, or a failover, must degrade rather than break — so
// an unavailable id falls back to the config-only scope.
func (s *RedisSessionStore) rolloutObservationScopeFingerprint() string {
	instance, err := s.currentRedisInstanceID()
	if err != nil {
		instance = ""
	}
	return s.scopeFingerprintWithInstance(instance)
}

func (s *RedisSessionStore) scopeFingerprintWithInstance(instance string) string {
	encoded := strconv.AppendQuote(nil, "token-session-observation/v1")
	encoded = append(encoded, '\n')
	encoded = strconv.AppendQuote(encoded, s.tokenPrefix)
	encoded = append(encoded, '\n')
	encoded = strconv.AppendQuote(encoded, s.uidTokenPrefix)
	encoded = append(encoded, '\n')
	encoded = strconv.AppendInt(encoded, s.maxTTL.Milliseconds(), 10)
	encoded = append(encoded, '\n')
	encoded = strconv.AppendQuote(encoded, instance)
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}

func (s *RedisSessionStore) loadPersistentEvidence(key string, target interface{}) error {
	raw, err := s.client.Get(key).Result()
	if err == rd.Nil {
		return errors.New("required evidence is missing")
	}
	if err != nil {
		return fmt.Errorf("load evidence: %w", err)
	}
	ttl, err := s.client.PTTL(key).Result()
	if err != nil {
		return fmt.Errorf("read evidence ttl: %w", err)
	}
	if ttl != -time.Millisecond {
		return errors.New("required evidence must not expire")
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("decode evidence: %w", err)
	}
	return nil
}

func (s *RedisSessionStore) latestMigrationCompletionKey() string {
	return s.uidTokenPrefix + "auth:migration:latest-complete"
}
