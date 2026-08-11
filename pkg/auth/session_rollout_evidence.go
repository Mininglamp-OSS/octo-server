package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	rd "github.com/go-redis/redis"
)

var appendRolloutObservationScript = rd.NewScript(`
local history = {}
local current = redis.call("GET", KEYS[1])
if current then
  history = cjson.decode(current)
end
local next = cjson.decode(ARGV[1])
for _, evidence in ipairs(history) do
  if evidence.id == next.id then
    return 0
  end
end
table.insert(history, next)
while #history > 2 do
  table.remove(history, 1)
end
redis.call("SET", KEYS[1], cjson.encode(history))
return 1
`)

type rolloutObservationEvidence struct {
	ID           string             `json:"id"`
	RecordedAtMS int64              `json:"recorded_at_unix_ms"`
	ModeFloor    SessionMode        `json:"mode_floor"`
	Observation  SessionObservation `json:"observation"`
}

type migrationCompletionEvidence struct {
	CampaignID      string `json:"campaign_id"`
	CompletedAtMS   int64  `json:"completed_at_unix_ms"`
	RedisInstanceID string `json:"redis_instance_id"`
}

// RecordRolloutObservation persists only aggregate, low-cardinality evidence
// for a later floor transition. It never stores a token, Redis key, UID, or
// payload. The last two observations share one non-expiring key and are
// appended atomically so concurrent operators cannot lose an evidence record.
func (s *RedisSessionStore) RecordRolloutObservation(ctx context.Context, observation SessionObservation) error {
	if err := validateRecordableRolloutObservation(observation); err != nil {
		return err
	}
	if observation.ScopeFingerprint != s.rolloutObservationScopeFingerprint() {
		return errors.New("auth: rollout observation evidence scope does not match this session store")
	}
	control, err := s.RolloutControl(ctx)
	if err != nil {
		return err
	}
	if control == nil || control.ModeFloor.rank() < SessionModeRevoke.rank() {
		return errors.New("auth: rollout observation evidence requires persisted revoke floor")
	}
	evidence := rolloutObservationEvidence{
		ID:           observation.ScanID,
		RecordedAtMS: s.now().UTC().UnixMilli(),
		ModeFloor:    control.ModeFloor,
		Observation:  observation,
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("auth: encode rollout observation evidence: %w", err)
	}
	result, err := appendRolloutObservationScript.Run(
		s.client,
		[]string{s.rolloutObservationEvidenceKey()},
		string(encoded),
	).Result()
	if err != nil {
		return fmt.Errorf("auth: record rollout observation evidence: %w", err)
	}
	appended, err := redisInteger(result)
	if err != nil {
		return fmt.Errorf("auth: record rollout observation evidence: %w", err)
	}
	if appended != 1 {
		return errors.New("auth: rollout observation scan was already recorded")
	}
	return nil
}

func validateRecordableRolloutObservation(observation SessionObservation) error {
	if !observation.Complete {
		return errors.New("auth: rollout observation evidence must be complete")
	}
	if observation.Total <= 0 {
		return errors.New("auth: rollout observation evidence must cover a non-empty token scope")
	}
	counts := []int64{
		observation.Missing,
		observation.Persistent,
		observation.Finite,
		observation.OverMax,
		observation.InvalidTTL,
		observation.DecodeInvalid,
		observation.ReadErrors,
		observation.V1,
		observation.V2,
		observation.V3,
	}
	for _, count := range counts {
		if count < 0 {
			return errors.New("auth: rollout observation evidence counts must not be negative")
		}
	}
	if observation.Missing+observation.Persistent+observation.Finite+observation.InvalidTTL != observation.Total ||
		observation.V1+observation.V2+observation.V3+observation.DecodeInvalid != observation.Total-observation.Missing ||
		observation.OverMax > observation.Finite {
		return errors.New("auth: rollout observation evidence counts do not match total")
	}
	if observation.ReadErrors != 0 || observation.InvalidTTL != 0 || observation.DecodeInvalid != 0 {
		return errors.New("auth: rollout observation evidence contains ambiguous token records")
	}
	if strings.TrimSpace(observation.ScanID) == "" || strings.TrimSpace(observation.ScopeFingerprint) == "" {
		return errors.New("auth: rollout observation evidence requires scan identity and scope fingerprint")
	}
	return nil
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

func (s *RedisSessionStore) validateRolloutAdvanceEvidence(ctx context.Context, current, next SessionMode, observationMinGapMS int64) error {
	if next != SessionModeBounded && next != SessionModeEnforce {
		return nil
	}
	if next == SessionModeBounded {
		completion, err := s.loadMigrationCompletionEvidence()
		if err != nil {
			return err
		}
		checkpoint, err := s.loadMigrationCheckpoint(completion.CampaignID)
		if err != nil {
			return err
		}
		if checkpoint == nil || !checkpoint.Result.Complete || checkpoint.Result.LockLost || checkpoint.CampaignID != completion.CampaignID {
			return errors.New("auth: bounded floor requires complete migration checkpoint evidence")
		}
		redisInstanceID, err := s.currentRedisInstanceID()
		if err != nil {
			return fmt.Errorf("auth: bounded floor verify Redis instance: %w", err)
		}
		if completion.RedisInstanceID == "" || checkpoint.RedisInstanceID != completion.RedisInstanceID || redisInstanceID != completion.RedisInstanceID {
			return errors.New("auth: bounded floor requires migration evidence from the current Redis instance")
		}
		if err := s.validateObservationEvidence(current, next, completion.CompletedAtMS, observationMinGapMS); err != nil {
			return err
		}
		return nil
	}
	return s.validateObservationEvidence(current, next, 0, observationMinGapMS)
}

func (s *RedisSessionStore) loadMigrationCompletionEvidence() (migrationCompletionEvidence, error) {
	var evidence migrationCompletionEvidence
	if err := s.loadPersistentEvidence(s.latestMigrationCompletionKey(), &evidence); err != nil {
		return migrationCompletionEvidence{}, fmt.Errorf("auth: bounded floor migration evidence: %w", err)
	}
	if evidence.CampaignID == "" || evidence.CompletedAtMS <= 0 || evidence.RedisInstanceID == "" {
		return migrationCompletionEvidence{}, errors.New("auth: bounded floor migration evidence is invalid")
	}
	return evidence, nil
}

func (s *RedisSessionStore) validateObservationEvidence(current, next SessionMode, notBeforeMS, observationMinGapMS int64) error {
	var history []rolloutObservationEvidence
	if err := s.loadPersistentEvidence(s.rolloutObservationEvidenceKey(), &history); err != nil {
		return fmt.Errorf("auth: %s floor observation evidence: %w", next, err)
	}
	if len(history) != 2 {
		return fmt.Errorf("auth: %s floor requires two complete observation evidence records", next)
	}
	seen := make(map[string]struct{}, len(history))
	for _, evidence := range history {
		if evidence.ID == "" || evidence.ModeFloor != current || evidence.RecordedAtMS < notBeforeMS {
			return fmt.Errorf("auth: %s floor requires two complete observations from current floor %s after migration", next, current)
		}
		if _, duplicate := seen[evidence.ID]; duplicate {
			return fmt.Errorf("auth: %s floor requires two distinct complete observations", next)
		}
		seen[evidence.ID] = struct{}{}
		if err := validateRecordableRolloutObservation(evidence.Observation); err != nil {
			return fmt.Errorf("auth: %s floor observation evidence is invalid: %w", next, err)
		}
		if evidence.ID != evidence.Observation.ScanID {
			return fmt.Errorf("auth: %s floor observation evidence scan identity does not match its record", next)
		}
		if evidence.Observation.ScopeFingerprint != s.rolloutObservationScopeFingerprint() {
			return fmt.Errorf("auth: %s floor observation evidence scope does not match this session store", next)
		}
		if evidence.Observation.Persistent != 0 || evidence.Observation.OverMax != 0 {
			return fmt.Errorf("auth: %s floor requires persistent=0 and over_max=0 evidence", next)
		}
		if next == SessionModeEnforce {
			if evidence.Observation.V3 <= 0 {
				return errors.New("auth: enforce floor requires v3 > 0 evidence")
			}
			if evidence.Observation.V1 != 0 || evidence.Observation.V2 != 0 {
				return errors.New("auth: enforce floor requires v1=0 and v2=0 evidence")
			}
		}
	}
	if observationMinGapMS <= 0 || history[1].RecordedAtMS-history[0].RecordedAtMS < observationMinGapMS {
		return fmt.Errorf("auth: %s floor observations must have the persisted minimum gap of %dms", next, observationMinGapMS)
	}
	return nil
}

func (s *RedisSessionStore) rolloutObservationScopeFingerprint() string {
	encoded := strconv.AppendQuote(nil, "token-session-observation/v1")
	encoded = append(encoded, '\n')
	encoded = strconv.AppendQuote(encoded, s.tokenPrefix)
	encoded = append(encoded, '\n')
	encoded = strconv.AppendQuote(encoded, s.uidTokenPrefix)
	encoded = append(encoded, '\n')
	encoded = strconv.AppendInt(encoded, s.maxTTL.Milliseconds(), 10)
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

func (s *RedisSessionStore) rolloutObservationEvidenceKey() string {
	return s.uidTokenPrefix + "auth:rollout-observations"
}
