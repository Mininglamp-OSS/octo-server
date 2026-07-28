package card_template_catalog

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/go-sql-driver/mysql"
)

const (
	claimSourceStatic  = "static"
	claimSourceDynamic = "dynamic"

	auditOperationPublish = "publish"

	maxActorUIDRunes     = 128
	maxReasonRunes       = 512
	maxChangeTicketRunes = 128

	publishAttempts   = 3
	publishRetryDelay = 50 * time.Millisecond
)

var (
	ErrImmutableVersionConflict = errors.New("card template version already exists with different content")
	ErrVersionClaimConflict     = errors.New("card template version claim conflicts with source")
	ErrCatalogIntegrity         = errors.New("card template catalog integrity failure")
	ErrPublishRequestInvalid    = errors.New("card template publish request invalid")
)

const (
	insertClaimSQL = `INSERT INTO card_template_version_claim
        (template_id, version, source) VALUES (?, ?, ?)`
	upsertStaticClaimSQL = `INSERT INTO card_template_version_claim
        (template_id, version, source) VALUES (?, ?, ?)
        ON DUPLICATE KEY UPDATE source = source`
	selectPublishedClaimSQL = `SELECT c.source, a.content_sha256, a.blocked_at IS NOT NULL
        FROM card_template_version_claim c
        LEFT JOIN card_template_artifact a
          ON a.template_id = c.template_id AND a.version = c.version
        WHERE c.template_id = ? AND c.version = ?
        FOR UPDATE`
	insertArtifactSQL = `INSERT INTO card_template_artifact
        (template_id, version, owner, visibility, engine_contract, protocol,
         contract_version, canonical_bundle, content_sha256, created_by)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	insertAuditSQL = `INSERT INTO card_template_audit
        (actor_uid, operation, template_id, version, content_sha256, result, reason, change_ticket)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	selectStaticClaimSQL = `SELECT c.source,
        EXISTS(SELECT 1 FROM card_template_artifact a
               WHERE a.template_id = c.template_id AND a.version = c.version) AS has_artifact
        FROM card_template_version_claim c
        WHERE c.template_id = ? AND c.version = ?
        FOR UPDATE`
)

type store struct {
	db *sql.DB
}

func newStore(db *sql.DB) *store { return &store{db: db} }

type PublishRequest struct {
	Artifact     *cardtmpl.CompiledArtifact
	ActorUID     string
	Reason       string
	ChangeTicket string
}

type PublishResult struct {
	Created    bool
	Idempotent bool
	Blocked    bool
}

func (s *store) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	if err := validatePublishRequest(request); err != nil {
		return PublishResult{}, err
	}
	var lastErr error
	for attempt := 1; attempt <= publishAttempts; attempt++ {
		result, err := s.publishOnce(ctx, request)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !isTransientPublishError(err) || attempt == publishAttempts {
			return PublishResult{}, err
		}
		if err := waitForPublishRetry(ctx, publishRetryDelay*time.Duration(1<<(attempt-1))); err != nil {
			return PublishResult{}, err
		}
	}
	return PublishResult{}, lastErr
}

func (s *store) publishOnce(ctx context.Context, request PublishRequest) (PublishResult, error) {
	artifact := request.Artifact
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PublishResult{}, fmt.Errorf("card template catalog: begin publish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, insertClaimSQL, string(artifact.Meta.ID), artifact.Meta.Version, claimSourceDynamic)
	if err != nil {
		if !isDuplicateKey(err) {
			return PublishResult{}, fmt.Errorf("card template catalog: claim version: %w", err)
		}
		result, resolveErr := s.resolveExistingPublish(ctx, tx, request)
		if resolveErr != nil {
			return PublishResult{}, resolveErr
		}
		if err := tx.Commit(); err != nil {
			return PublishResult{}, fmt.Errorf("card template catalog: commit idempotent publish: %w", err)
		}
		return result, nil
	}

	if _, err := tx.ExecContext(ctx, insertArtifactSQL,
		string(artifact.Meta.ID), artifact.Meta.Version, artifact.Owner, artifact.Visibility,
		artifact.Engine, artifact.Meta.Protocol, artifact.ContractVersion, []byte(artifact.Bundle),
		artifact.Hash, request.ActorUID,
	); err != nil {
		return PublishResult{}, fmt.Errorf("card template catalog: insert artifact: %w", err)
	}
	if err := insertPublishAudit(ctx, tx, request, "created"); err != nil {
		return PublishResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PublishResult{}, fmt.Errorf("card template catalog: commit publish: %w", err)
	}
	return PublishResult{Created: true}, nil
}

func isTransientPublishError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}
	return mysqlErr.Number == 1205 || mysqlErr.Number == 1213
}

func waitForPublishRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *store) resolveExistingPublish(
	ctx context.Context,
	tx *sql.Tx,
	request PublishRequest,
) (PublishResult, error) {
	artifact := request.Artifact
	var source string
	var storedHash sql.NullString
	var blocked bool
	err := tx.QueryRowContext(ctx, selectPublishedClaimSQL, string(artifact.Meta.ID), artifact.Meta.Version).
		Scan(&source, &storedHash, &blocked)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PublishResult{}, fmt.Errorf("%w: load existing claim: %w", ErrCatalogIntegrity, err)
		}
		return PublishResult{}, fmt.Errorf("card template catalog: load existing claim: %w", err)
	}
	if source != claimSourceDynamic {
		return PublishResult{}, fmt.Errorf("%w: %s@%s is claimed by %s",
			ErrVersionClaimConflict, artifact.Meta.ID, artifact.Meta.Version, source)
	}
	if !storedHash.Valid || storedHash.String == "" {
		return PublishResult{}, fmt.Errorf("%w: dynamic claim %s@%s has no artifact",
			ErrCatalogIntegrity, artifact.Meta.ID, artifact.Meta.Version)
	}
	if storedHash.String != artifact.Hash {
		return PublishResult{}, fmt.Errorf("%w: %s@%s", ErrImmutableVersionConflict, artifact.Meta.ID, artifact.Meta.Version)
	}
	if err := insertPublishAudit(ctx, tx, request, "idempotent"); err != nil {
		return PublishResult{}, err
	}
	return PublishResult{Idempotent: true, Blocked: blocked}, nil
}

func insertPublishAudit(ctx context.Context, tx *sql.Tx, request PublishRequest, result string) error {
	artifact := request.Artifact
	if _, err := tx.ExecContext(ctx, insertAuditSQL,
		request.ActorUID, auditOperationPublish, string(artifact.Meta.ID), artifact.Meta.Version,
		artifact.Hash, result, request.Reason, request.ChangeTicket,
	); err != nil {
		return fmt.Errorf("card template catalog: insert publish audit: %w", err)
	}
	return nil
}

func (s *store) ReconcileStatic(ctx context.Context, inventory []cardtmpl.TemplateMeta) error {
	ordered := append([]cardtmpl.TemplateMeta(nil), inventory...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ID != ordered[j].ID {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Version < ordered[j].Version
	})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("card template catalog: begin static reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var previous string
	for _, meta := range ordered {
		identity := string(meta.ID) + "@" + meta.Version
		if strings.TrimSpace(string(meta.ID)) == "" || strings.TrimSpace(meta.Version) == "" || identity == previous {
			return fmt.Errorf("%w: invalid or duplicate static identity %q", ErrCatalogIntegrity, identity)
		}
		previous = identity
		if _, err := tx.ExecContext(ctx, upsertStaticClaimSQL, string(meta.ID), meta.Version, claimSourceStatic); err != nil {
			return fmt.Errorf("card template catalog: claim static %s: %w", identity, err)
		}
		var source string
		var hasArtifact bool
		if err := tx.QueryRowContext(ctx, selectStaticClaimSQL, string(meta.ID), meta.Version).
			Scan(&source, &hasArtifact); err != nil {
			return fmt.Errorf("card template catalog: load static claim %s: %w", identity, err)
		}
		if source != claimSourceStatic {
			return fmt.Errorf("%w: static %s is claimed by %s", ErrVersionClaimConflict, identity, source)
		}
		if hasArtifact {
			return fmt.Errorf("%w: static claim %s unexpectedly has dynamic artifact", ErrCatalogIntegrity, identity)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("card template catalog: commit static reconciliation: %w", err)
	}
	return nil
}

func validatePublishRequest(request PublishRequest) error {
	artifact := request.Artifact
	if artifact == nil || strings.TrimSpace(string(artifact.Meta.ID)) == "" || strings.TrimSpace(artifact.Meta.Version) == "" {
		return fmt.Errorf("%w: artifact identity is required", ErrPublishRequestInvalid)
	}
	if artifact.Engine != cardtmpl.JSONTemplateEngineV1 ||
		(artifact.Visibility != cardtmpl.CatalogVisibilityPublic && artifact.Visibility != cardtmpl.CatalogVisibilityPrivate) ||
		strings.TrimSpace(artifact.Owner) == "" || artifact.Meta.Protocol != cardtmpl.Protocol {
		return fmt.Errorf("%w: artifact metadata is invalid", ErrPublishRequestInvalid)
	}
	decodedHash, err := hex.DecodeString(artifact.Hash)
	if err != nil || len(decodedHash) != sha256Bytes || strings.ToLower(artifact.Hash) != artifact.Hash {
		return fmt.Errorf("%w: artifact hash is invalid", ErrPublishRequestInvalid)
	}
	if len(artifact.Bundle) == 0 || len(artifact.Bundle) > maxCanonicalBundleBytes {
		return fmt.Errorf("%w: canonical bundle size is invalid", ErrPublishRequestInvalid)
	}
	if !boundedRequiredText(request.ActorUID, maxActorUIDRunes) ||
		!boundedRequiredText(request.Reason, maxReasonRunes) ||
		!boundedRequiredText(request.ChangeTicket, maxChangeTicketRunes) {
		return fmt.Errorf("%w: actor, reason, and change_ticket are required and bounded", ErrPublishRequestInvalid)
	}
	return nil
}

const (
	sha256Bytes             = 32
	maxCanonicalBundleBytes = 2 << 20
)

func boundedRequiredText(value string, maxRunes int) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed != "" && trimmed == value && len([]rune(value)) <= maxRunes
}

func isDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
