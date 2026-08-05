package card_template_catalog

// PR-C D4 — the one-snapshot authorization resolver.
//
// Everything one dynamic authorization decision depends on is read here, in a
// single REPEATABLE READ transaction against the primary: the activation
// pointer, the claim/artifact/block row for the version that pointer resolves
// to, and the principal's exact + global grant rows. InnoDB gives that
// transaction one consistent snapshot, so an activate, block or revoke that
// commits while the transaction is open cannot show up in half of the answer.
//
// The transaction is the linearization point. A request whose snapshot began
// before a revoke committed is allowed to finish; a request whose snapshot
// begins after it must see the tombstone and be denied. There is no third
// outcome, and in particular there is no "read the pointer now, check the
// grant later" path left in the codebase for one to leak back through.
//
// Nothing here is cached. Grant truth lives only in MySQL (D4: no Redis, no
// local grant cache in v1); the compiled-artifact cache upstream keys on the
// immutable content hash and therefore holds no activation, block or grant
// state at all.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

// Compile-time proof that the production store really is the indivisible
// resolver. If this ever stops holding, RuntimeCatalog silently degrades to
// the grant-unaware split-read path and every dynamic business access starts
// failing closed — loud in tests, but this assertion catches it at build time.
var _ cardtmpl.RuntimeAuthorizationStore = (*store)(nil)

const (
	// selectAuthorizationGrantsSQL fetches at most the two rows that can decide
	// one principal's access: the row scoped to this Space and the global row.
	// Both are needed because an exact tombstone must be able to shadow an
	// active global grant, which a "first match wins" query could not express.
	selectAuthorizationGrantsSQL = `SELECT scope_space_id, status, can_discover, can_send, can_edit, revision
        FROM card_template_grant
        WHERE template_id = ? AND principal_type = ? AND principal_id = ?
          AND scope_space_id IN (?, '')`
	// selectPrincipalGrantsSQL lists a principal's grant rows across templates
	// for the Bot advertised set. It is bounded by the caller and covers both
	// scopes so the same precedence rule applies per template ID.
	selectPrincipalGrantsSQL = `SELECT template_id, scope_space_id, status,
            can_discover, can_send, can_edit, revision
        FROM card_template_grant
        WHERE principal_type = ? AND principal_id = ?
          AND scope_space_id IN (?, '')
        ORDER BY template_id ASC
        LIMIT ?`
)

// maxAuthorizedTemplates bounds the per-principal advertised set. A Bot with
// more granted templates than this is a control-plane problem, not something
// to silently paginate inside a capability manifest.
const maxAuthorizedTemplates = 64

// grantPrincipal maps a runtime principal onto the grant table's identity
// space. `system` and blank identities are not grantable business principals
// (D2), so they resolve to no lookup at all rather than to an empty-string
// principal that could accidentally match a row.
func grantPrincipal(principal cardtmpl.CatalogPrincipal) (GrantPrincipalType, string, bool) {
	id := strings.TrimSpace(principal.ID)
	if id == "" {
		return "", "", false
	}
	switch principal.Kind {
	case cardtmpl.CatalogPrincipalBot:
		return GrantPrincipalBot, id, true
	case cardtmpl.CatalogPrincipalInternalProducer:
		return GrantPrincipalInternalProducer, id, true
	default:
		return "", "", false
	}
}

// LoadAuthorization answers one query from one snapshot.
func (s *store) LoadAuthorization(
	ctx context.Context,
	query cardtmpl.RuntimeAuthorizationQuery,
) (cardtmpl.RuntimeAuthorization, error) {
	if strings.TrimSpace(string(query.ID)) == "" {
		return cardtmpl.RuntimeAuthorization{},
			fmt.Errorf("%w: authorization query has no template", cardtmpl.ErrTemplateUnknown)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return cardtmpl.RuntimeAuthorization{}, fmt.Errorf("card template catalog: begin authorization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result := cardtmpl.RuntimeAuthorization{Version: query.Version}
	if query.Version == "" {
		activation, err := scanRuntimeActivation(ctx, tx, query.ID)
		if err != nil {
			return cardtmpl.RuntimeAuthorization{}, err
		}
		version, err := cardtmpl.ActivationVersion(query.ID, activation)
		if err != nil {
			return cardtmpl.RuntimeAuthorization{}, err
		}
		result.Activation, result.Version = activation, version
	}
	if result.Version != "" {
		meta, err := scanRuntimeArtifactMeta(ctx, tx, query.ID, result.Version)
		if err != nil {
			return cardtmpl.RuntimeAuthorization{}, err
		}
		result.Artifact = meta
	}
	grant, err := loadGrantDecision(ctx, tx, query.ID, query.Principal)
	if err != nil {
		return cardtmpl.RuntimeAuthorization{}, err
	}
	result.Grant = grant

	// Commit a read-only transaction rather than relying on the deferred
	// rollback: it releases the snapshot deterministically and turns a
	// connection-level failure into an error instead of a silently truncated
	// read that would look like "no grant" — a denial we could not distinguish
	// from a real one.
	if err := tx.Commit(); err != nil {
		return cardtmpl.RuntimeAuthorization{}, fmt.Errorf("card template catalog: commit authorization: %w", err)
	}
	return result, nil
}

// loadGrantDecision reads both candidate rows and reduces them through the one
// precedence implementation shared with the manager control plane.
func loadGrantDecision(
	ctx context.Context,
	tx *sql.Tx,
	id cardtmpl.ID,
	principal cardtmpl.CatalogPrincipal,
) (cardtmpl.RuntimeGrant, error) {
	principalType, principalID, grantable := grantPrincipal(principal)
	if !grantable {
		return cardtmpl.RuntimeGrant{}, nil
	}
	scope := strings.TrimSpace(principal.SpaceID)
	rows, err := tx.QueryContext(ctx, selectAuthorizationGrantsSQL,
		string(id), string(principalType), principalID, scope)
	if err != nil {
		return cardtmpl.RuntimeGrant{}, fmt.Errorf("card template catalog: load authorization grants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var exact, global *GrantRecord
	for rows.Next() {
		record, err := scanGrantScopeRow(rows, id, principalType, principalID)
		if err != nil {
			return cardtmpl.RuntimeGrant{}, err
		}
		// A blank scope is the canonical global sentinel. When the principal
		// itself carries no Space, the IN list collapses and the single row
		// returned is the global one — never an "exact" match on emptiness.
		if record.Identity.ScopeSpaceID == "" {
			global = &record
			continue
		}
		if record.Identity.ScopeSpaceID != scope {
			return cardtmpl.RuntimeGrant{}, fmt.Errorf("%w: grant scope %q is outside the query",
				ErrCatalogIntegrity, record.Identity.ScopeSpaceID)
		}
		exact = &record
	}
	if err := rows.Err(); err != nil {
		return cardtmpl.RuntimeGrant{}, fmt.Errorf("card template catalog: iterate authorization grants: %w", err)
	}
	return runtimeGrantFromDecision(resolveGrantRows(exact, global)), nil
}

func runtimeGrantFromDecision(decision GrantDecision) cardtmpl.RuntimeGrant {
	if decision.ScopeSource == "" {
		return cardtmpl.RuntimeGrant{}
	}
	return cardtmpl.RuntimeGrant{
		Found:    true,
		Scope:    cardtmpl.RuntimeGrantScope(decision.ScopeSource),
		Revision: decision.Revision,
		Discover: decision.Allowed.Discover,
		Send:     decision.Allowed.Send,
		Edit:     decision.Allowed.Edit,
	}
}

func scanGrantScopeRow(
	rows *sql.Rows,
	id cardtmpl.ID,
	principalType GrantPrincipalType,
	principalID string,
) (GrantRecord, error) {
	record := GrantRecord{Identity: GrantIdentity{
		TemplateID: id, PrincipalType: principalType, PrincipalID: principalID,
	}}
	var status string
	if err := rows.Scan(&record.Identity.ScopeSpaceID, &status,
		&record.Permissions.Discover, &record.Permissions.Send, &record.Permissions.Edit,
		&record.Revision,
	); err != nil {
		return GrantRecord{}, fmt.Errorf("card template catalog: scan authorization grant: %w", err)
	}
	record.Status = GrantStatus(status)
	if record.Status != GrantStatusActive && record.Status != GrantStatusRevoked {
		return GrantRecord{}, fmt.Errorf("%w: invalid grant status %q", ErrCatalogIntegrity, status)
	}
	return record, nil
}

// ListAuthorizedTemplates returns every template ID the principal holds an
// effective grant on, together with that template's activation and artifact
// state — all from one snapshot, for the same reason LoadAuthorization uses
// one. Templates whose pointer is absent, disabled or integrity-broken are
// skipped rather than failing the whole listing: one bad template must not
// take a Bot's entire capability manifest down with it.
//
// Purpose filtering is deliberately left to the caller. This returns the
// snapshot; deciding what `send` or `edit` means for it stays with the single
// authorizer, so the permission rule is never re-implemented here.
func (s *store) ListAuthorizedTemplates(
	ctx context.Context,
	principal cardtmpl.CatalogPrincipal,
	limit int,
) ([]cardtmpl.RuntimeAdvertisedTemplate, error) {
	principalType, principalID, grantable := grantPrincipal(principal)
	if !grantable {
		return nil, nil
	}
	if limit < 1 || limit > maxAuthorizedTemplates {
		limit = maxAuthorizedTemplates
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("card template catalog: begin authorized templates: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	scope := strings.TrimSpace(principal.SpaceID)
	// Two scopes per template, so read twice the row budget before reducing.
	decisions, err := scanPrincipalGrants(ctx, tx, principalType, principalID, scope, limit*2)
	if err != nil {
		return nil, err
	}
	results := make([]cardtmpl.RuntimeAdvertisedTemplate, 0, len(decisions))
	for _, id := range sortedTemplateIDs(decisions) {
		grant := runtimeGrantFromDecision(decisions[id])
		if !grant.Discover && !grant.Send && !grant.Edit {
			continue
		}
		authorization, ok, err := loadActiveAuthorization(ctx, tx, id, grant)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		results = append(results, cardtmpl.RuntimeAdvertisedTemplate{ID: id, Authorization: authorization})
		if len(results) == limit {
			break
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("card template catalog: commit authorized templates: %w", err)
	}
	return results, nil
}

// loadActiveAuthorization completes one listed template's snapshot. A pointer
// that is absent, disabled or malformed yields ok=false: the template simply is
// not advertisable right now, which is different from an error.
func loadActiveAuthorization(
	ctx context.Context,
	tx *sql.Tx,
	id cardtmpl.ID,
	grant cardtmpl.RuntimeGrant,
) (cardtmpl.RuntimeAuthorization, bool, error) {
	activation, err := scanRuntimeActivation(ctx, tx, id)
	if err != nil {
		if errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
			return cardtmpl.RuntimeAuthorization{}, false, nil
		}
		return cardtmpl.RuntimeAuthorization{}, false, err
	}
	version, err := cardtmpl.ActivationVersion(id, activation)
	if err != nil || version == "" {
		return cardtmpl.RuntimeAuthorization{}, false, nil
	}
	meta, err := scanRuntimeArtifactMeta(ctx, tx, id, version)
	if err != nil {
		if errors.Is(err, cardtmpl.ErrTemplateUnknown) || errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
			return cardtmpl.RuntimeAuthorization{}, false, nil
		}
		return cardtmpl.RuntimeAuthorization{}, false, err
	}
	return cardtmpl.RuntimeAuthorization{
		Activation: activation, Version: version, Artifact: meta, Grant: grant,
	}, true, nil
}

func scanPrincipalGrants(
	ctx context.Context,
	tx *sql.Tx,
	principalType GrantPrincipalType,
	principalID, scope string,
	rowLimit int,
) (map[cardtmpl.ID]GrantDecision, error) {
	rows, err := tx.QueryContext(ctx, selectPrincipalGrantsSQL,
		string(principalType), principalID, scope, rowLimit)
	if err != nil {
		return nil, fmt.Errorf("card template catalog: list principal grants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	exact := make(map[cardtmpl.ID]GrantRecord)
	global := make(map[cardtmpl.ID]GrantRecord)
	for rows.Next() {
		var templateID string
		var scopeSpaceID, status string
		record := GrantRecord{}
		if err := rows.Scan(&templateID, &scopeSpaceID, &status,
			&record.Permissions.Discover, &record.Permissions.Send, &record.Permissions.Edit,
			&record.Revision,
		); err != nil {
			return nil, fmt.Errorf("card template catalog: scan principal grant: %w", err)
		}
		record.Status = GrantStatus(status)
		if record.Status != GrantStatusActive && record.Status != GrantStatusRevoked {
			return nil, fmt.Errorf("%w: invalid grant status %q", ErrCatalogIntegrity, status)
		}
		id := cardtmpl.ID(templateID)
		record.Identity = GrantIdentity{
			TemplateID: id, PrincipalType: principalType,
			PrincipalID: principalID, ScopeSpaceID: scopeSpaceID,
		}
		if scopeSpaceID == "" {
			global[id] = record
			continue
		}
		exact[id] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("card template catalog: iterate principal grants: %w", err)
	}

	decisions := make(map[cardtmpl.ID]GrantDecision, len(exact)+len(global))
	for id := range exact {
		record := exact[id]
		var globalRow *GrantRecord
		if row, ok := global[id]; ok {
			globalRow = &row
		}
		decisions[id] = resolveGrantRows(&record, globalRow)
	}
	for id := range global {
		if _, resolved := decisions[id]; resolved {
			continue
		}
		record := global[id]
		decisions[id] = resolveGrantRows(nil, &record)
	}
	return decisions, nil
}

func sortedTemplateIDs(decisions map[cardtmpl.ID]GrantDecision) []cardtmpl.ID {
	ids := make([]cardtmpl.ID, 0, len(decisions))
	for id := range decisions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
