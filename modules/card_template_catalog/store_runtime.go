package card_template_catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

const (
	selectRuntimeActivationSQL = `SELECT active_version, status, revision
        FROM card_template_activation
        WHERE template_id = ?`
	selectRuntimeArtifactMetaSQL = `SELECT c.source, a.owner, a.visibility, a.engine_contract,
        a.protocol, a.contract_version, a.content_sha256, a.blocked_at IS NOT NULL
        FROM card_template_version_claim c
        LEFT JOIN card_template_artifact a
          ON a.template_id = c.template_id AND a.version = c.version
        WHERE c.template_id = ? AND c.version = ?`
	selectRuntimeArtifactBundleSQL = `SELECT canonical_bundle FROM card_template_artifact
        WHERE template_id = ? AND version = ? AND content_sha256 = ?`
)

// rowQuerier lets the activation/artifact readers run either standalone or
// inside the single authorization snapshot without a second copy of the shape
// validation below. *sql.DB and *sql.Tx both satisfy it.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *store) LoadActivation(ctx context.Context, id cardtmpl.ID) (cardtmpl.RuntimeActivation, error) {
	return scanRuntimeActivation(ctx, s.db, id)
}

func scanRuntimeActivation(
	ctx context.Context,
	querier rowQuerier,
	id cardtmpl.ID,
) (cardtmpl.RuntimeActivation, error) {
	var version sql.NullString
	var status string
	var revision uint64
	err := querier.QueryRowContext(ctx, selectRuntimeActivationSQL, string(id)).
		Scan(&version, &status, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return cardtmpl.RuntimeActivation{}, nil
	}
	if err != nil {
		return cardtmpl.RuntimeActivation{}, fmt.Errorf("card template catalog: load runtime activation: %w", err)
	}
	return buildRuntimeActivation(version, status, revision)
}

// buildRuntimeActivation validates one activation row's shape. It is separate
// from the query so the single-row read and the batched advertised-set read
// share one copy — two copies of a shape check are two chances to disagree
// about what a malformed row means.
func buildRuntimeActivation(
	version sql.NullString,
	status string,
	revision uint64,
) (cardtmpl.RuntimeActivation, error) {
	result := cardtmpl.RuntimeActivation{Exists: true, Version: version.String, Revision: revision}
	switch status {
	case string(activationStatusActive):
		if !version.Valid || version.String == "" {
			return cardtmpl.RuntimeActivation{}, fmt.Errorf("%w: active row has no version", cardtmpl.ErrRuntimeCatalogIntegrity)
		}
		result.Status = cardtmpl.RuntimeActivationActive
	case string(activationStatusDisabled):
		if version.Valid {
			return cardtmpl.RuntimeActivation{}, fmt.Errorf("%w: disabled row has a version", cardtmpl.ErrRuntimeCatalogIntegrity)
		}
		result.Status = cardtmpl.RuntimeActivationDisabled
	default:
		return cardtmpl.RuntimeActivation{}, fmt.Errorf("%w: invalid activation status", cardtmpl.ErrRuntimeCatalogIntegrity)
	}
	return result, nil
}

func (s *store) LoadArtifactMeta(
	ctx context.Context,
	id cardtmpl.ID,
	version string,
) (cardtmpl.RuntimeArtifactMeta, error) {
	return scanRuntimeArtifactMeta(ctx, s.db, id, version)
}

func scanRuntimeArtifactMeta(
	ctx context.Context,
	querier rowQuerier,
	id cardtmpl.ID,
	version string,
) (cardtmpl.RuntimeArtifactMeta, error) {
	var source string
	var owner, visibility, engine, protocol, contractVersion, hash sql.NullString
	var blocked bool
	err := querier.QueryRowContext(ctx, selectRuntimeArtifactMetaSQL, string(id), version).Scan(
		&source, &owner, &visibility, &engine, &protocol, &contractVersion, &hash, &blocked,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return cardtmpl.RuntimeArtifactMeta{}, fmt.Errorf("%w: %s@%s", cardtmpl.ErrTemplateUnknown, id, version)
	}
	if err != nil {
		return cardtmpl.RuntimeArtifactMeta{}, fmt.Errorf("card template catalog: load runtime artifact metadata: %w", err)
	}
	return buildRuntimeArtifactMeta(id, version, source,
		artifactMetaColumns{owner, visibility, engine, protocol, contractVersion, hash}, blocked)
}

// artifactMetaColumns groups the nullable artifact columns so the shared shape
// check below reads as one thing rather than as six positional arguments.
type artifactMetaColumns struct {
	owner           sql.NullString
	visibility      sql.NullString
	engine          sql.NullString
	protocol        sql.NullString
	contractVersion sql.NullString
	hash            sql.NullString
}

// buildRuntimeArtifactMeta validates one claim/artifact pair, shared by the
// single-row read and the batched advertised-set read for the same reason
// buildRuntimeActivation is.
func buildRuntimeArtifactMeta(
	id cardtmpl.ID,
	version, source string,
	columns artifactMetaColumns,
	blocked bool,
) (cardtmpl.RuntimeArtifactMeta, error) {
	owner, visibility, engine := columns.owner, columns.visibility, columns.engine
	protocol, contractVersion, hash := columns.protocol, columns.contractVersion, columns.hash
	result := cardtmpl.RuntimeArtifactMeta{ID: id, Version: version, Blocked: blocked}
	switch source {
	case claimSourceStatic:
		result.Source = cardtmpl.RuntimeSourceStatic
		if owner.Valid || visibility.Valid || engine.Valid || protocol.Valid || contractVersion.Valid || hash.Valid {
			return cardtmpl.RuntimeArtifactMeta{}, fmt.Errorf("%w: static claim has dynamic artifact", cardtmpl.ErrRuntimeCatalogIntegrity)
		}
		return result, nil
	case claimSourceDynamic:
		if !owner.Valid || !visibility.Valid || !engine.Valid || !protocol.Valid || !contractVersion.Valid || !hash.Valid {
			return cardtmpl.RuntimeArtifactMeta{}, fmt.Errorf("%w: dynamic claim has no complete artifact", cardtmpl.ErrRuntimeCatalogIntegrity)
		}
		result.Source = cardtmpl.RuntimeSourceDynamic
	default:
		return cardtmpl.RuntimeArtifactMeta{}, fmt.Errorf("%w: unknown claim source", cardtmpl.ErrRuntimeCatalogIntegrity)
	}
	result.Owner = owner.String
	result.Visibility = visibility.String
	result.Engine = engine.String
	result.Protocol = protocol.String
	result.ContractVersion = contractVersion.String
	result.Hash = hash.String
	return result, nil
}

func (s *store) LoadArtifactBundle(
	ctx context.Context,
	id cardtmpl.ID,
	version string,
	hash string,
) (cardtmpl.CanonicalBundle, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, selectRuntimeArtifactBundleSQL, string(id), version, hash).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: artifact bundle identity/hash mismatch", cardtmpl.ErrRuntimeCatalogIntegrity)
	}
	if err != nil {
		return nil, fmt.Errorf("card template catalog: load runtime artifact bundle: %w", err)
	}
	if len(raw) == 0 || len(raw) > maxCanonicalBundleBytes {
		return nil, fmt.Errorf("%w: artifact bundle size is invalid", cardtmpl.ErrRuntimeCatalogIntegrity)
	}
	return append(cardtmpl.CanonicalBundle(nil), raw...), nil
}
