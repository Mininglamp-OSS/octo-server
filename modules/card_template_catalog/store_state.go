package card_template_catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

type activationStatus string

const (
	activationStatusActive   activationStatus = "active"
	activationStatusDisabled activationStatus = "disabled"

	auditOperationActivate = "activate"
	auditOperationRollback = "rollback"
	auditOperationBlock    = "block"

	stateTransitionAttempts = 3
)

var (
	ErrActivationConflict      = errors.New("card template activation revision conflict")
	ErrActiveTargetCapacity    = errors.New("card template active target capacity reached")
	ErrActivationTargetInvalid = errors.New("card template activation target invalid")
	ErrArtifactBlocked         = errors.New("card template artifact is blocked")
	ErrBlockTargetInvalid      = errors.New("card template block target invalid")
	ErrRollbackTargetInvalid   = errors.New("card template rollback target was never active")
)

const (
	selectActivationForUpdateSQL = `SELECT active_version, status, revision
        FROM card_template_activation
        WHERE template_id = ?
        FOR UPDATE`
	selectCapacityGuardForUpdateSQL = `SELECT guard_key FROM card_template_catalog_capacity_guard
        WHERE guard_key = 1
        FOR UPDATE`
	countActiveTargetsSQL = `SELECT COUNT(*) FROM card_template_activation
        WHERE status = 'active'`
	selectStateTargetForUpdateSQL = `SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL,
		a.owner, a.visibility, a.engine_contract, a.protocol, a.contract_version, a.content_sha256
        FROM card_template_version_claim c
        LEFT JOIN card_template_artifact a
          ON a.template_id = c.template_id AND a.version = c.version
        WHERE c.template_id = ? AND c.version = ?
        FOR UPDATE`
	insertActivationSQL = `INSERT INTO card_template_activation
        (template_id, active_version, status, revision, updated_by, reason, change_ticket)
        VALUES (?, ?, ?, 1, ?, ?, ?)`
	updateActivationSQL = `UPDATE card_template_activation
        SET active_version = ?, status = ?, revision = revision + 1,
            updated_by = ?, reason = ?, change_ticket = ?, updated_at = CURRENT_TIMESTAMP(6)
        WHERE template_id = ? AND revision = ?`
	blockArtifactSQL = `UPDATE card_template_artifact
        SET blocked_at = CURRENT_TIMESTAMP(6), blocked_by = ?, block_reason = ?
        WHERE template_id = ? AND version = ? AND blocked_at IS NULL`
	selectPriorActiveSQL = `SELECT EXISTS(SELECT 1 FROM card_template_audit
        WHERE template_id = ? AND result = 'ok'
          AND operation IN ('activate', 'rollback', 'block')
          AND (previous_version = ? OR resulting_version = ?))`
	insertStateAuditSQL = `INSERT INTO card_template_audit
        (actor_uid, operation, template_id, version, previous_version, resulting_version,
         previous_revision, resulting_revision, content_sha256, result, reason, change_ticket)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', 'ok', ?, ?)`
)

type ActivationRequest struct {
	TemplateID       cardtmpl.ID
	TargetVersion    string
	ExpectedRevision uint64
	ActorUID         string
	Reason           string
	ChangeTicket     string
	targetReceipt    stateTargetReceipt
}

type ActivationResult struct {
	TemplateID       cardtmpl.ID
	PreviousVersion  string
	ActiveVersion    string
	Status           activationStatus
	PreviousRevision uint64
	Revision         uint64
}

type RollbackRequest struct {
	TemplateID       cardtmpl.ID
	TargetVersion    string
	ExpectedRevision uint64
	ActorUID         string
	Reason           string
	ChangeTicket     string
	targetReceipt    stateTargetReceipt
}

type BlockRequest struct {
	TemplateID       cardtmpl.ID
	Version          string
	ExpectedRevision uint64
	FallbackVersion  string
	ActorUID         string
	Reason           string
	ChangeTicket     string
	fallbackReceipt  stateTargetReceipt
}

type BlockResult struct {
	TemplateID       cardtmpl.ID
	Version          string
	Blocked          bool
	PreviousVersion  string
	ActiveVersion    string
	Status           activationStatus
	PreviousRevision uint64
	Revision         uint64
}

type activationRow struct {
	exists   bool
	version  string
	status   activationStatus
	revision uint64
}

type stateTarget struct {
	source      string
	hasArtifact bool
	blocked     bool
	meta        cardtmpl.RuntimeArtifactMeta
}

type stateTargetReceipt struct {
	meta cardtmpl.RuntimeArtifactMeta
}

func (s *store) Activate(ctx context.Context, request ActivationRequest) (ActivationResult, error) {
	if err := validateActivationRequest(request); err != nil {
		return ActivationResult{}, err
	}
	return executeStateWithRetry(ctx, func() (ActivationResult, error) {
		return s.activateOnce(ctx, request)
	})
}

func (s *store) activateOnce(ctx context.Context, request ActivationRequest) (ActivationResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ActivationResult{}, fmt.Errorf("card template catalog: begin activate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := loadActivationForUpdate(ctx, tx, request.TemplateID)
	if err != nil {
		return ActivationResult{}, err
	}
	if current.revision != request.ExpectedRevision {
		return ActivationResult{}, fmt.Errorf("%w: expected %d, current %d",
			ErrActivationConflict, request.ExpectedRevision, current.revision)
	}
	if !current.exists || current.status == activationStatusDisabled {
		if err := requireActiveTargetCapacity(ctx, tx); err != nil {
			return ActivationResult{}, err
		}
	}
	target, err := loadStateTargetForUpdate(ctx, tx, request.TemplateID, request.TargetVersion)
	if err != nil {
		return ActivationResult{}, err
	}
	if err := requireStateTargetReceipt(target, request.targetReceipt); err != nil {
		return ActivationResult{}, err
	}

	nextRevision := current.revision + 1
	if !current.exists {
		if _, err := tx.ExecContext(ctx, insertActivationSQL,
			string(request.TemplateID), request.TargetVersion, activationStatusActive,
			request.ActorUID, request.Reason, request.ChangeTicket,
		); err != nil {
			if isDuplicateKey(err) {
				return ActivationResult{}, fmt.Errorf("%w: concurrent first activation", ErrActivationConflict)
			}
			return ActivationResult{}, fmt.Errorf("card template catalog: insert activation: %w", err)
		}
	} else {
		result, err := tx.ExecContext(ctx, updateActivationSQL,
			request.TargetVersion, activationStatusActive, request.ActorUID, request.Reason,
			request.ChangeTicket, string(request.TemplateID), request.ExpectedRevision,
		)
		if err != nil {
			return ActivationResult{}, fmt.Errorf("card template catalog: update activation: %w", err)
		}
		if err := requireOneStateRow(result); err != nil {
			return ActivationResult{}, err
		}
	}
	if err := insertStateAudit(ctx, tx, stateAudit{
		actorUID: request.ActorUID, operation: auditOperationActivate,
		templateID: request.TemplateID, version: request.TargetVersion,
		previousVersion: current.version, resultingVersion: request.TargetVersion,
		previousRevision: current.revision, resultingRevision: nextRevision,
		reason: request.Reason, changeTicket: request.ChangeTicket,
	}); err != nil {
		return ActivationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ActivationResult{}, fmt.Errorf("card template catalog: commit activate: %w", err)
	}
	return ActivationResult{
		TemplateID: request.TemplateID, PreviousVersion: current.version,
		ActiveVersion: request.TargetVersion, Status: activationStatusActive,
		PreviousRevision: current.revision, Revision: nextRevision,
	}, nil
}

func (s *store) Block(ctx context.Context, request BlockRequest) (BlockResult, error) {
	if err := validateBlockRequest(request); err != nil {
		return BlockResult{}, err
	}
	return executeStateWithRetry(ctx, func() (BlockResult, error) {
		return s.blockOnce(ctx, request)
	})
}

func (s *store) blockOnce(ctx context.Context, request BlockRequest) (BlockResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BlockResult{}, fmt.Errorf("card template catalog: begin block: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := loadActivationForUpdate(ctx, tx, request.TemplateID)
	if err != nil {
		return BlockResult{}, err
	}
	if current.revision != request.ExpectedRevision {
		return BlockResult{}, fmt.Errorf("%w: expected %d, current %d",
			ErrActivationConflict, request.ExpectedRevision, current.revision)
	}
	if current.exists && current.status == activationStatusActive &&
		current.version == request.Version && request.FallbackVersion == "" {
		if err := lockActiveTargetCapacityGuard(ctx, tx); err != nil {
			return BlockResult{}, err
		}
	}
	target, err := loadStateTargetForUpdate(ctx, tx, request.TemplateID, request.Version)
	if err != nil {
		return BlockResult{}, err
	}
	if target.source != claimSourceDynamic || !target.hasArtifact {
		return BlockResult{}, fmt.Errorf("%w: %s@%s is not a dynamic artifact",
			ErrBlockTargetInvalid, request.TemplateID, request.Version)
	}
	if target.blocked {
		return BlockResult{}, fmt.Errorf("%w: %s@%s", ErrArtifactBlocked, request.TemplateID, request.Version)
	}
	blockResult, err := tx.ExecContext(ctx, blockArtifactSQL,
		request.ActorUID, request.Reason, string(request.TemplateID), request.Version)
	if err != nil {
		return BlockResult{}, fmt.Errorf("card template catalog: block artifact: %w", err)
	}
	if err := requireOneBlockedRow(blockResult); err != nil {
		return BlockResult{}, err
	}

	next := current
	if current.exists && current.status == activationStatusActive && current.version == request.Version {
		nextVersion := request.FallbackVersion
		nextStatus := activationStatusActive
		var persistedVersion any = nextVersion
		if nextVersion == "" {
			nextStatus = activationStatusDisabled
			persistedVersion = nil
		} else {
			fallback, err := loadStateTargetForUpdate(ctx, tx, request.TemplateID, nextVersion)
			if err != nil {
				return BlockResult{}, fmt.Errorf("card template catalog: validate block fallback: %w", err)
			}
			if err := requireStateTargetReceipt(fallback, request.fallbackReceipt); err != nil {
				return BlockResult{}, err
			}
			if fallback.source == claimSourceDynamic {
				previouslyActive, err := targetWasPreviouslyActive(ctx, tx, request.TemplateID, nextVersion)
				if err != nil {
					return BlockResult{}, err
				}
				if !previouslyActive {
					return BlockResult{}, fmt.Errorf("%w: dynamic fallback %s@%s was never active",
						ErrBlockTargetInvalid, request.TemplateID, nextVersion)
				}
			}
		}
		stateResult, err := tx.ExecContext(ctx, updateActivationSQL,
			persistedVersion, nextStatus, request.ActorUID, request.Reason, request.ChangeTicket,
			string(request.TemplateID), request.ExpectedRevision,
		)
		if err != nil {
			return BlockResult{}, fmt.Errorf("card template catalog: update blocked activation: %w", err)
		}
		if err := requireOneStateRow(stateResult); err != nil {
			return BlockResult{}, err
		}
		next.version = nextVersion
		next.status = nextStatus
		next.revision++
	}
	if err := insertStateAudit(ctx, tx, stateAudit{
		actorUID: request.ActorUID, operation: auditOperationBlock,
		templateID: request.TemplateID, version: request.Version,
		previousVersion: current.version, resultingVersion: next.version,
		previousRevision: current.revision, resultingRevision: next.revision,
		reason: request.Reason, changeTicket: request.ChangeTicket,
	}); err != nil {
		return BlockResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return BlockResult{}, fmt.Errorf("card template catalog: commit block: %w", err)
	}
	return BlockResult{
		TemplateID: request.TemplateID, Version: request.Version, Blocked: true,
		PreviousVersion: current.version, ActiveVersion: next.version, Status: next.status,
		PreviousRevision: current.revision, Revision: next.revision,
	}, nil
}

func (s *store) Rollback(ctx context.Context, request RollbackRequest) (ActivationResult, error) {
	activation := ActivationRequest{
		TemplateID: request.TemplateID, TargetVersion: request.TargetVersion,
		ExpectedRevision: request.ExpectedRevision, ActorUID: request.ActorUID,
		Reason: request.Reason, ChangeTicket: request.ChangeTicket,
		targetReceipt: request.targetReceipt,
	}
	if err := validateActivationRequest(activation); err != nil {
		return ActivationResult{}, err
	}
	return executeStateWithRetry(ctx, func() (ActivationResult, error) {
		return s.rollbackOnce(ctx, request)
	})
}

func (s *store) rollbackOnce(ctx context.Context, request RollbackRequest) (ActivationResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ActivationResult{}, fmt.Errorf("card template catalog: begin rollback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := loadActivationForUpdate(ctx, tx, request.TemplateID)
	if err != nil {
		return ActivationResult{}, err
	}
	if !current.exists || current.revision != request.ExpectedRevision {
		return ActivationResult{}, fmt.Errorf("%w: expected %d, current %d",
			ErrActivationConflict, request.ExpectedRevision, current.revision)
	}
	if current.status == activationStatusDisabled {
		if err := requireActiveTargetCapacity(ctx, tx); err != nil {
			return ActivationResult{}, err
		}
	}
	target, err := loadStateTargetForUpdate(ctx, tx, request.TemplateID, request.TargetVersion)
	if err != nil {
		return ActivationResult{}, err
	}
	if err := requireStateTargetReceipt(target, request.targetReceipt); err != nil {
		return ActivationResult{}, err
	}
	previouslyActive, err := targetWasPreviouslyActive(ctx, tx, request.TemplateID, request.TargetVersion)
	if err != nil {
		return ActivationResult{}, err
	}
	if !previouslyActive {
		return ActivationResult{}, fmt.Errorf("%w: %s@%s", ErrRollbackTargetInvalid, request.TemplateID, request.TargetVersion)
	}
	nextRevision := current.revision + 1
	result, err := tx.ExecContext(ctx, updateActivationSQL,
		request.TargetVersion, activationStatusActive, request.ActorUID, request.Reason,
		request.ChangeTicket, string(request.TemplateID), request.ExpectedRevision,
	)
	if err != nil {
		return ActivationResult{}, fmt.Errorf("card template catalog: update rollback: %w", err)
	}
	if err := requireOneStateRow(result); err != nil {
		return ActivationResult{}, err
	}
	if err := insertStateAudit(ctx, tx, stateAudit{
		actorUID: request.ActorUID, operation: auditOperationRollback,
		templateID: request.TemplateID, version: request.TargetVersion,
		previousVersion: current.version, resultingVersion: request.TargetVersion,
		previousRevision: current.revision, resultingRevision: nextRevision,
		reason: request.Reason, changeTicket: request.ChangeTicket,
	}); err != nil {
		return ActivationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ActivationResult{}, fmt.Errorf("card template catalog: commit rollback: %w", err)
	}
	return ActivationResult{
		TemplateID: request.TemplateID, PreviousVersion: current.version,
		ActiveVersion: request.TargetVersion, Status: activationStatusActive,
		PreviousRevision: current.revision, Revision: nextRevision,
	}, nil
}

func executeStateWithRetry[T any](ctx context.Context, operation func() (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 1; attempt <= stateTransitionAttempts; attempt++ {
		result, err := operation()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !isTransientPublishError(err) || attempt == stateTransitionAttempts {
			return zero, err
		}
		if err := waitForPublishRetry(ctx, publishRetryDelay*time.Duration(1<<(attempt-1))); err != nil {
			return zero, err
		}
	}
	return zero, lastErr
}

func targetWasPreviouslyActive(
	ctx context.Context,
	tx *sql.Tx,
	id cardtmpl.ID,
	version string,
) (bool, error) {
	var previouslyActive bool
	if err := tx.QueryRowContext(ctx, selectPriorActiveSQL, string(id), version, version).
		Scan(&previouslyActive); err != nil {
		return false, fmt.Errorf("card template catalog: verify active history: %w", err)
	}
	return previouslyActive, nil
}

func loadActivationForUpdate(ctx context.Context, tx *sql.Tx, id cardtmpl.ID) (activationRow, error) {
	var version sql.NullString
	var status activationStatus
	var revision uint64
	err := tx.QueryRowContext(ctx, selectActivationForUpdateSQL, string(id)).Scan(&version, &status, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return activationRow{}, nil
	}
	if err != nil {
		return activationRow{}, fmt.Errorf("card template catalog: load activation: %w", err)
	}
	if status != activationStatusActive && status != activationStatusDisabled {
		return activationRow{}, fmt.Errorf("%w: invalid activation status %q", ErrCatalogIntegrity, status)
	}
	if (status == activationStatusActive) != version.Valid {
		return activationRow{}, fmt.Errorf("%w: invalid activation shape", ErrCatalogIntegrity)
	}
	return activationRow{exists: true, version: version.String, status: status, revision: revision}, nil
}

func requireActiveTargetCapacity(ctx context.Context, tx *sql.Tx) error {
	if err := lockActiveTargetCapacityGuard(ctx, tx); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRowContext(ctx, countActiveTargetsSQL).Scan(&active); err != nil {
		return fmt.Errorf("card template catalog: count active targets: %w", err)
	}
	if active >= startupActiveTargetLimit {
		return fmt.Errorf("%w: hard limit %d", ErrActiveTargetCapacity, startupActiveTargetLimit)
	}
	return nil
}

func lockActiveTargetCapacityGuard(ctx context.Context, tx *sql.Tx) error {
	var guardKey uint8
	err := tx.QueryRowContext(ctx, selectCapacityGuardForUpdateSQL).Scan(&guardKey)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: active target capacity guard is missing", ErrCatalogIntegrity)
	}
	if err != nil {
		return fmt.Errorf("card template catalog: lock active target capacity: %w", err)
	}
	if guardKey != 1 {
		return fmt.Errorf("%w: invalid active target capacity guard", ErrCatalogIntegrity)
	}
	return nil
}

func loadStateTargetForUpdate(
	ctx context.Context,
	tx *sql.Tx,
	id cardtmpl.ID,
	version string,
) (stateTarget, error) {
	var target stateTarget
	var owner, visibility, engine, protocol, contractVersion, hash sql.NullString
	err := tx.QueryRowContext(ctx, selectStateTargetForUpdateSQL, string(id), version).
		Scan(&target.source, &target.hasArtifact, &target.blocked,
			&owner, &visibility, &engine, &protocol, &contractVersion, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return stateTarget{}, fmt.Errorf("%w: %s@%s not claimed", ErrActivationTargetInvalid, id, version)
	}
	if err != nil {
		return stateTarget{}, fmt.Errorf("card template catalog: load state target: %w", err)
	}
	switch target.source {
	case claimSourceStatic:
		if target.hasArtifact || owner.Valid || visibility.Valid || engine.Valid || protocol.Valid || contractVersion.Valid || hash.Valid {
			return stateTarget{}, fmt.Errorf("%w: static target has dynamic artifact", ErrCatalogIntegrity)
		}
	case claimSourceDynamic:
		if !target.hasArtifact || !owner.Valid || !visibility.Valid || !engine.Valid || !protocol.Valid || !contractVersion.Valid || !hash.Valid {
			return stateTarget{}, fmt.Errorf("%w: dynamic target has incomplete artifact", ErrCatalogIntegrity)
		}
		target.meta = cardtmpl.RuntimeArtifactMeta{
			ID: id, Version: version, Source: cardtmpl.RuntimeSourceDynamic,
			Owner: owner.String, Visibility: visibility.String, Engine: engine.String,
			Protocol: protocol.String, ContractVersion: contractVersion.String, Hash: hash.String,
		}
	default:
		return stateTarget{}, fmt.Errorf("%w: unknown source %q", ErrCatalogIntegrity, target.source)
	}
	if target.blocked {
		return stateTarget{}, fmt.Errorf("%w: %s@%s", ErrArtifactBlocked, id, version)
	}
	return target, nil
}

func requireStateTargetReceipt(target stateTarget, receipt stateTargetReceipt) error {
	meta := receipt.meta
	if meta.ID == "" || meta.Version == "" {
		return fmt.Errorf("%w: target validation receipt is missing", ErrCatalogIntegrity)
	}
	switch target.source {
	case claimSourceStatic:
		if meta.Source != cardtmpl.RuntimeSourceStatic {
			return fmt.Errorf("%w: static target receipt source mismatch", ErrCatalogIntegrity)
		}
		if target.meta.ID != "" || target.meta.Version != "" {
			return fmt.Errorf("%w: static target contains dynamic metadata", ErrCatalogIntegrity)
		}
	case claimSourceDynamic:
		if meta.Source != cardtmpl.RuntimeSourceDynamic || meta.ID != target.meta.ID || meta.Version != target.meta.Version ||
			meta.Engine != target.meta.Engine || meta.Hash != target.meta.Hash || meta.Owner != target.meta.Owner ||
			meta.Visibility != target.meta.Visibility || meta.Protocol != target.meta.Protocol ||
			meta.ContractVersion != target.meta.ContractVersion || meta.Blocked {
			return fmt.Errorf("%w: target changed after validation", ErrCatalogIntegrity)
		}
	default:
		return fmt.Errorf("%w: unknown target source", ErrCatalogIntegrity)
	}
	return nil
}

type stateAudit struct {
	actorUID          string
	operation         string
	templateID        cardtmpl.ID
	version           string
	previousVersion   string
	resultingVersion  string
	previousRevision  uint64
	resultingRevision uint64
	reason            string
	changeTicket      string
}

func insertStateAudit(ctx context.Context, tx *sql.Tx, audit stateAudit) error {
	if _, err := tx.ExecContext(ctx, insertStateAuditSQL,
		audit.actorUID, audit.operation, string(audit.templateID), audit.version,
		audit.previousVersion, audit.resultingVersion, audit.previousRevision,
		audit.resultingRevision, audit.reason, audit.changeTicket,
	); err != nil {
		return fmt.Errorf("card template catalog: insert %s audit: %w", audit.operation, err)
	}
	return nil
}

func validateActivationRequest(request ActivationRequest) error {
	if strings.TrimSpace(string(request.TemplateID)) == "" ||
		strings.TrimSpace(request.TargetVersion) == "" || request.TargetVersion != strings.TrimSpace(request.TargetVersion) ||
		!boundedRequiredText(request.ActorUID, maxActorUIDRunes) ||
		!boundedRequiredText(request.Reason, maxReasonRunes) ||
		!boundedRequiredText(request.ChangeTicket, maxChangeTicketRunes) {
		return fmt.Errorf("%w: activation fields are required and bounded", ErrPublishRequestInvalid)
	}
	if request.targetReceipt.meta.ID != request.TemplateID || request.targetReceipt.meta.Version != request.TargetVersion {
		return fmt.Errorf("%w: activation target validation receipt is missing", ErrCatalogIntegrity)
	}
	return nil
}

func validateBlockRequest(request BlockRequest) error {
	if strings.TrimSpace(string(request.TemplateID)) == "" ||
		strings.TrimSpace(request.Version) == "" || request.Version != strings.TrimSpace(request.Version) ||
		(request.FallbackVersion != "" && request.FallbackVersion != strings.TrimSpace(request.FallbackVersion)) ||
		request.FallbackVersion == request.Version ||
		!boundedRequiredText(request.ActorUID, maxActorUIDRunes) ||
		!boundedRequiredText(request.Reason, maxReasonRunes) ||
		!boundedRequiredText(request.ChangeTicket, maxChangeTicketRunes) {
		return fmt.Errorf("%w: block fields are required and bounded", ErrPublishRequestInvalid)
	}
	if request.FallbackVersion != "" &&
		(request.fallbackReceipt.meta.ID != request.TemplateID || request.fallbackReceipt.meta.Version != request.FallbackVersion) {
		return fmt.Errorf("%w: block fallback validation receipt is missing", ErrCatalogIntegrity)
	}
	return nil
}

func requireOneStateRow(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("card template catalog: read activation CAS result: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: activation CAS affected %d rows", ErrActivationConflict, affected)
	}
	return nil
}

func requireOneBlockedRow(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("card template catalog: read block result: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: block affected %d rows", ErrArtifactBlocked, affected)
	}
	return nil
}
