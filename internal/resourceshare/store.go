package resourceshare

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"time"

	"github.com/gocraft/dbr/v2"
	"github.com/gowebpki/jcs"
)

type DeliveryState string

const (
	DeliveryClaimed     DeliveryState = "claimed"
	DeliveryRateLimited DeliveryState = "rate_limited"
	DeliveryFailed      DeliveryState = "failed"
	DeliveryDenied      DeliveryState = "denied"
	DeliveryDispatching DeliveryState = "dispatching"
	DeliverySent        DeliveryState = "sent"
	DeliveryUnknown     DeliveryState = "unknown"
)

type IntentClaimResult struct {
	IntentID    int64
	Disposition ReplayDisposition
}

type DeliveryRecord struct {
	ID          int64
	IntentID    int64
	DeliveryID  string
	TargetKind  TargetKind
	TargetRef   string
	State       DeliveryState
	RetryAt     int64
	MessageID   string
	MessageSeq  uint32
	ClientMsgNo string
	OutcomeCode string
	CreatedAt   int64
	UpdatedAt   int64
}

type DeliveryClaimResult struct {
	Record  DeliveryRecord
	Created bool
}

type TransportResult struct {
	MessageID   string
	MessageSeq  uint32
	ClientMsgNo string
}

type DurableStore struct {
	session *dbr.Session
	now     func() time.Time
}

func NewDurableStore(session *dbr.Session) *DurableStore {
	return &DurableStore{session: session, now: time.Now}
}

func (s *DurableStore) ClaimIntent(ctx context.Context, verified VerifiedIntent) (*IntentClaimResult, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if err := validateVerifiedForStore(verified); err != nil {
		return nil, err
	}
	nonceHash := sha256.Sum256([]byte(verified.Intent.Nonce))
	idempotencyHash := sha256.Sum256([]byte(verified.Intent.IdempotencyKey))
	now := s.now().Unix()
	result, insertErr := s.session.InsertInto("resource_share_intent").
		Columns(
			"nonce_hash", "fingerprint", "idempotency_hash", "actor_uid", "space_id",
			"provider_id", "resource_type", "resource_id", "resource_revision", "expires_at", "created_at",
		).
		Values(
			nonceHash[:], verified.Fingerprint[:], idempotencyHash[:], verified.Intent.ActorUID,
			verified.Intent.SpaceID, verified.ProviderID, verified.Intent.Resource.Type,
			verified.Intent.Resource.ID, verified.Intent.Resource.Revision, verified.Intent.ExpiresAt, now,
		).
		ExecContext(ctx)
	if insertErr == nil {
		intentID, err := result.LastInsertId()
		if err != nil {
			return nil, storeError("read inserted intent id", err)
		}
		return &IntentClaimResult{IntentID: intentID, Disposition: ReplayFirstUse}, nil
	}

	var existing struct {
		ID          int64
		Fingerprint []byte
	}
	err := s.session.Select("id", "fingerprint").
		From("resource_share_intent").
		Where("nonce_hash=?", nonceHash[:]).
		Limit(1).
		LoadOneContext(ctx, &existing)
	if err != nil {
		return nil, storeError("resolve intent claim after insert failure", insertErr)
	}
	if len(existing.Fingerprint) != sha256.Size {
		return nil, storeError("stored intent fingerprint length", errors.New("corrupt fingerprint"))
	}
	var stored IntentFingerprint
	copy(stored[:], existing.Fingerprint)
	disposition, err := ClassifyReplay(&stored, verified.Fingerprint)
	if err != nil {
		return nil, err
	}
	return &IntentClaimResult{IntentID: existing.ID, Disposition: disposition}, nil
}

func DeliveryIdentity(intent Intent, target Target) (string, error) {
	if !validIdentifier(intent.ActorUID, 1, maxActorUIDBytes) ||
		!validIdentifier(intent.SpaceID, 1, maxSpaceIDBytes) ||
		!providerIDPattern.MatchString(string(intent.Provider)) ||
		!providerIDPattern.MatchString(intent.Resource.Type) ||
		!validIdentifier(intent.Resource.ID, 1, maxResourceIDBytes) ||
		!validIdentifier(intent.Resource.Revision, 1, maxRevisionBytes) {
		return "", fmt.Errorf("%w: delivery identity input invalid", ErrIntentInvalid)
	}
	targetKey, err := canonicalTargetKey(intent.ActorUID, target)
	if err != nil {
		return "", fmt.Errorf("%w: delivery target invalid", ErrIntentInvalid)
	}
	digest := sha256.New()
	for _, field := range []string{
		intent.ActorUID,
		intent.SpaceID,
		string(intent.Provider),
		intent.Resource.Type,
		intent.Resource.ID,
		intent.Resource.Revision,
		targetKey,
	} {
		writeDigestField(digest, field)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (s *DurableStore) ClaimDelivery(
	ctx context.Context,
	intentID int64,
	verified VerifiedIntent,
	target Target,
	requestID string,
) (*DeliveryClaimResult, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if intentID <= 0 || !validAuditValue(requestID, 0, 128) {
		return nil, storeError("invalid delivery claim", errors.New("invalid intent or request id"))
	}
	if err := validateVerifiedForStore(verified); err != nil {
		return nil, err
	}
	deliveryID, err := DeliveryIdentity(verified.Intent, target)
	if err != nil {
		return nil, err
	}
	targetRef, err := canonicalTargetReference(verified.Intent.ActorUID, target)
	if err != nil {
		return nil, err
	}
	now := s.now().Unix()
	tx, err := s.session.BeginTx(ctx, nil)
	if err != nil {
		return nil, storeError("begin delivery claim", err)
	}
	defer tx.RollbackUnlessCommitted()
	if err := verifyIntentBindingTx(ctx, tx, intentID, verified.Fingerprint); err != nil {
		return nil, err
	}

	result, insertErr := tx.InsertInto("resource_share_delivery").
		Columns(
			"intent_id", "delivery_id", "target_kind", "target_ref", "state", "retry_at",
			"message_id", "message_seq", "client_msg_no", "outcome_code", "created_at", "updated_at",
		).
		Values(
			intentID, deliveryID, target.Kind, targetRef, DeliveryClaimed, 0,
			"", 0, "", "", now, now,
		).
		ExecContext(ctx)
	if insertErr != nil {
		_ = tx.Rollback()
		existing, loadErr := s.loadDeliveryByIdentity(ctx, deliveryID)
		if loadErr != nil {
			return nil, storeError("resolve delivery claim after insert failure", insertErr)
		}
		return &DeliveryClaimResult{Record: *existing, Created: false}, nil
	}
	deliveryRowID, err := result.LastInsertId()
	if err != nil {
		return nil, storeError("read inserted delivery id", err)
	}
	if err := insertDeliveryAudit(ctx, tx, deliveryRowID, requestID, DeliveryClaimed, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, storeError("commit delivery claim", err)
	}
	record := DeliveryRecord{
		ID: deliveryRowID, IntentID: intentID, DeliveryID: deliveryID, TargetKind: target.Kind,
		TargetRef: targetRef, State: DeliveryClaimed, CreatedAt: now, UpdatedAt: now,
	}
	return &DeliveryClaimResult{Record: record, Created: true}, nil
}

func verifyIntentBindingTx(
	ctx context.Context,
	tx *dbr.Tx,
	intentID int64,
	fingerprint IntentFingerprint,
) error {
	var storedBytes []byte
	err := tx.Select("fingerprint").
		From("resource_share_intent").
		Where("id=?", intentID).
		Limit(1).
		LoadOneContext(ctx, &storedBytes)
	if err != nil {
		return storeError("load intent binding", err)
	}
	if len(storedBytes) != sha256.Size {
		return storeError("stored intent binding length", errors.New("corrupt fingerprint"))
	}
	var stored IntentFingerprint
	copy(stored[:], storedBytes)
	_, err = ClassifyReplay(&stored, fingerprint)
	return err
}

func (s *DurableStore) BeginDispatch(ctx context.Context, deliveryRowID int64, requestID string) error {
	return s.transition(ctx, deliveryTransition{
		ID: deliveryRowID, From: DeliveryClaimed, To: DeliveryDispatching, RequestID: requestID,
	})
}

func (s *DurableStore) MarkSent(ctx context.Context, deliveryRowID int64, result TransportResult, requestID string) error {
	messageID, err := strconv.ParseInt(result.MessageID, 10, 64)
	if err != nil || messageID <= 0 || result.MessageSeq == 0 ||
		!validAuditValue(result.ClientMsgNo, 0, 128) {
		return storeError("invalid sent transport result", errors.New("invalid message result"))
	}
	return s.transition(ctx, deliveryTransition{
		ID: deliveryRowID, From: DeliveryDispatching, To: DeliverySent, RequestID: requestID,
		MessageID: result.MessageID, MessageSeq: result.MessageSeq, ClientMsgNo: result.ClientMsgNo,
	})
}

func (s *DurableStore) MarkUnknown(ctx context.Context, deliveryRowID int64, outcomeCode, requestID string) error {
	if !validAuditValue(outcomeCode, 1, 64) {
		return storeError("invalid unknown outcome", errors.New("invalid outcome code"))
	}
	return s.transition(ctx, deliveryTransition{
		ID: deliveryRowID, From: DeliveryDispatching, To: DeliveryUnknown,
		RequestID: requestID, OutcomeCode: outcomeCode,
	})
}

func (s *DurableStore) RecordPreTransportOutcome(
	ctx context.Context,
	deliveryRowID int64,
	state DeliveryState,
	retryAt time.Time,
	outcomeCode, requestID string,
) error {
	if state != DeliveryDenied && state != DeliveryRateLimited && state != DeliveryFailed {
		return storeError("invalid pre-transport state", errors.New("unsupported state"))
	}
	if !validAuditValue(outcomeCode, 1, 64) {
		return storeError("invalid pre-transport outcome", errors.New("invalid outcome code"))
	}
	retryUnix := int64(0)
	if state == DeliveryRateLimited || state == DeliveryFailed {
		if retryAt.IsZero() || !retryAt.After(s.now()) {
			return storeError("invalid retry boundary", errors.New("retry must be in the future"))
		}
		retryUnix = retryAt.Unix()
	} else if !retryAt.IsZero() {
		return storeError("denied outcome cannot retry", errors.New("unexpected retry boundary"))
	}
	return s.transition(ctx, deliveryTransition{
		ID: deliveryRowID, From: DeliveryClaimed, To: state, RetryAt: retryUnix,
		RequestID: requestID, OutcomeCode: outcomeCode,
	})
}

func (s *DurableStore) LoadDelivery(ctx context.Context, deliveryRowID int64) (*DeliveryRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var record DeliveryRecord
	err := s.session.Select(
		"id", "intent_id", "delivery_id", "target_kind", "target_ref", "state", "retry_at",
		"message_id", "message_seq", "client_msg_no", "outcome_code", "created_at", "updated_at",
	).From("resource_share_delivery").Where("id=?", deliveryRowID).Limit(1).LoadOneContext(ctx, &record)
	if err != nil {
		return nil, storeError("load delivery", err)
	}
	return &record, nil
}

func (s *DurableStore) loadDeliveryByIdentity(ctx context.Context, deliveryID string) (*DeliveryRecord, error) {
	var record DeliveryRecord
	err := s.session.Select(
		"id", "intent_id", "delivery_id", "target_kind", "target_ref", "state", "retry_at",
		"message_id", "message_seq", "client_msg_no", "outcome_code", "created_at", "updated_at",
	).From("resource_share_delivery").Where("delivery_id=?", deliveryID).Limit(1).LoadOneContext(ctx, &record)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

type deliveryTransition struct {
	ID          int64
	From        DeliveryState
	To          DeliveryState
	RetryAt     int64
	MessageID   string
	MessageSeq  uint32
	ClientMsgNo string
	OutcomeCode string
	RequestID   string
}

func (s *DurableStore) transition(ctx context.Context, transition deliveryTransition) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if transition.ID <= 0 || !validAuditValue(transition.RequestID, 0, 128) {
		return storeError("invalid delivery transition", errors.New("invalid id or request id"))
	}
	now := s.now().Unix()
	tx, err := s.session.BeginTx(ctx, nil)
	if err != nil {
		return storeError("begin delivery transition", err)
	}
	defer tx.RollbackUnlessCommitted()
	result, err := tx.Update("resource_share_delivery").
		Set("state", transition.To).
		Set("retry_at", transition.RetryAt).
		Set("message_id", transition.MessageID).
		Set("message_seq", transition.MessageSeq).
		Set("client_msg_no", transition.ClientMsgNo).
		Set("outcome_code", transition.OutcomeCode).
		Set("updated_at", now).
		Where("id=? AND state=?", transition.ID, transition.From).
		ExecContext(ctx)
	if err != nil {
		return storeError("update delivery transition", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return storeError("read delivery transition result", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: expected state %s", ErrDeliveryConflict, transition.From)
	}
	if err := insertDeliveryAudit(ctx, tx, transition.ID, transition.RequestID, transition.To, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return storeError("commit delivery transition", err)
	}
	return nil
}

func insertDeliveryAudit(
	ctx context.Context,
	tx *dbr.Tx,
	deliveryRowID int64,
	requestID string,
	outcome DeliveryState,
	now int64,
) error {
	result, err := tx.ExecContext(ctx,
		"INSERT INTO resource_share_audit ("+
			"intent_id, delivery_id, actor_uid, space_id, provider_id, resource_type, resource_id, resource_revision, "+
			"target_kind, target_ref, request_id, outcome, created_at"+
			") SELECT i.id, d.delivery_id, i.actor_uid, i.space_id, i.provider_id, i.resource_type, i.resource_id, "+
			"i.resource_revision, d.target_kind, d.target_ref, ?, ?, ? "+
			"FROM resource_share_delivery d JOIN resource_share_intent i ON i.id=d.intent_id WHERE d.id=?",
		requestID, outcome, now, deliveryRowID,
	)
	if err != nil {
		return storeError("insert delivery audit", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return storeError("read audit insert result", err)
	}
	if affected != 1 {
		return storeError("insert delivery audit", errors.New("delivery row unavailable"))
	}
	return nil
}

func canonicalTargetReference(actorUID string, target Target) (string, error) {
	canonical, err := proofTargetForSend(actorUID, target)
	if err != nil {
		return "", fmt.Errorf("%w: canonical target reference: %v", ErrIntentInvalid, err)
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode target reference: %v", ErrIntentInvalid, err)
	}
	transformed, err := jcs.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("%w: canonicalize target reference: %v", ErrIntentInvalid, err)
	}
	return string(transformed), nil
}

func validateVerifiedForStore(verified VerifiedIntent) error {
	if verified.Fingerprint == (IntentFingerprint{}) ||
		verified.ProviderID == "" || verified.ProviderID != verified.Intent.Provider ||
		!validOpaque(verified.Intent.Nonce, minNonceBytes, maxNonceBytes) ||
		!validOpaque(verified.Intent.IdempotencyKey, minIdempotencyKeyBytes, maxIdempotencyKeyBytes) {
		return fmt.Errorf("%w: verified intent claim invalid", ErrIntentInvalid)
	}
	return nil
}

func (s *DurableStore) ready(ctx context.Context) error {
	if ctx == nil {
		return storeError("context unavailable", errors.New("nil context"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.session == nil || s.now == nil {
		return storeError("store unavailable", errors.New("missing dependency"))
	}
	return nil
}

func writeDigestField(digest hash.Hash, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write([]byte(value))
}

func validAuditValue(value string, minBytes, maxBytes int) bool {
	if len(value) < minBytes || len(value) > maxBytes {
		return false
	}
	if value == "" {
		return minBytes == 0
	}
	return validOpaque(value, minBytes, maxBytes)
}

func storeError(operation string, cause error) error {
	return fmt.Errorf("%w: %s: %w", ErrStore, operation, cause)
}
