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
	// The secondary sort key is not cosmetic: a template's exact and global rows
	// must be adjacent and in a defined order so the row budget can be truncated
	// at a template boundary rather than through the middle of a pair.
	selectPrincipalGrantsSQL = `SELECT template_id, scope_space_id, status,
            can_discover, can_send, can_edit, revision
        FROM card_template_grant
        WHERE principal_type = ? AND principal_id = ?
          AND scope_space_id IN (?, '')
        ORDER BY template_id ASC, scope_space_id ASC
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
	case cardtmpl.CatalogPrincipalSpace:
		return GrantPrincipalSpace, id, true
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

// LoadAuthorizations answers the same question as LoadAuthorization for a whole
// set of templates, from one snapshot and a bounded number of statements.
//
// It exists for the capability manifest, which asks about every ID in a static
// policy list on every poll. One transaction per ID turned a feature-detection
// read into N round trips, and a deployment whose Bots are user-created —
// thousands of them — multiplies that into steady primary-DB load for an answer
// that mostly has not changed.
//
// The semantics are LoadAuthorization's, deliberately and exactly: a template
// with no activation row yields a zero Version (the caller keeps its static
// behaviour), and a malformed activation or artifact is an error rather than a
// silent omission. That last part is what separates this from
// ListAuthorizedTemplates, which skips a broken template because one bad row
// must not empty an entire listing; here a broken row must fail the manifest
// closed, exactly as the per-ID loop did.
func (s *store) LoadAuthorizations(
	ctx context.Context,
	ids []cardtmpl.ID,
	principal cardtmpl.CatalogPrincipal,
) (map[cardtmpl.ID]cardtmpl.RuntimeAuthorization, error) {
	out := make(map[cardtmpl.ID]cardtmpl.RuntimeAuthorization, len(ids))
	unique := make([]cardtmpl.ID, 0, len(ids))
	seen := make(map[cardtmpl.ID]struct{}, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(string(id)) == "" {
			return nil, fmt.Errorf("%w: authorization query has no template", cardtmpl.ErrTemplateUnknown)
		}
		if _, done := seen[id]; done {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("card template catalog: begin batch authorization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	pointers, err := scanActivationPointers(ctx, tx, unique)
	if err != nil {
		return nil, err
	}
	grants, err := scanScopedGrantDecisions(ctx, tx, unique, principal)
	if err != nil {
		return nil, err
	}
	for _, id := range unique {
		result := cardtmpl.RuntimeAuthorization{Grant: grants[id]}
		if pointer, ok := pointers[id]; ok {
			version, err := cardtmpl.ActivationVersion(id, pointer.activation)
			if err != nil {
				return nil, err
			}
			result.Activation, result.Version = pointer.activation, version
			if version != "" {
				if version != pointer.version {
					// The pointer moved between the two halves of one snapshot,
					// which cannot happen inside REPEATABLE READ. Refuse rather
					// than serve an artifact from a version nobody pointed at.
					return nil, fmt.Errorf("%w: activation pointer disagrees with its joined artifact",
						cardtmpl.ErrRuntimeCatalogIntegrity)
				}
				meta, err := buildRuntimeArtifactMeta(id, version, pointer.source, pointer.columns, pointer.blocked)
				if err != nil {
					return nil, err
				}
				result.Artifact = meta
			}
		}
		out[id] = result
	}
	// Commit for the same reason the single read does: a connection-level
	// failure must surface as an error, not as a truncated read that reads like
	// a denial.
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("card template catalog: commit batch authorization: %w", err)
	}
	return out, nil
}

// activationPointer is one template's activation row joined to the artifact it
// points at, as the batch read returns them together.
type activationPointer struct {
	activation cardtmpl.RuntimeActivation
	version    string
	source     string
	columns    artifactMetaColumns
	blocked    bool
}

// selectActivationPointersSQL is the batch form of the activation + artifact
// reads. The join is LEFT so a disabled pointer (NULL active_version) still
// produces its activation row: the caller has to distinguish "disabled" from
// "no activation row at all", and an inner join would erase that difference.
const selectActivationPointersSQL = `SELECT act.template_id, act.active_version, act.status, act.revision,
        c.source, a.owner, a.visibility, a.engine_contract, a.protocol,
        a.contract_version, a.content_sha256, a.blocked_at IS NOT NULL
    FROM card_template_activation act
    LEFT JOIN card_template_version_claim c
      ON c.template_id = act.template_id AND c.version = act.active_version
    LEFT JOIN card_template_artifact a
      ON a.template_id = c.template_id AND a.version = c.version
    WHERE act.template_id IN (`

func scanActivationPointers(
	ctx context.Context,
	tx *sql.Tx,
	ids []cardtmpl.ID,
) (map[cardtmpl.ID]activationPointer, error) {
	query, args := inListQuery(selectActivationPointersSQL, ids)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("card template catalog: load activation pointers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[cardtmpl.ID]activationPointer, len(ids))
	for rows.Next() {
		var templateID, status string
		var activeVersion, source sql.NullString
		var revision uint64
		var columns artifactMetaColumns
		var blocked bool
		if err := rows.Scan(&templateID, &activeVersion, &status, &revision, &source,
			&columns.owner, &columns.visibility, &columns.engine, &columns.protocol,
			&columns.contractVersion, &columns.hash, &blocked); err != nil {
			return nil, fmt.Errorf("card template catalog: scan activation pointer: %w", err)
		}
		activation, err := buildRuntimeActivation(activeVersion, status, revision)
		if err != nil {
			return nil, err
		}
		if activeVersion.Valid && activeVersion.String != "" && !source.Valid {
			// An active pointer whose claim row is missing is the integrity
			// failure scanRuntimeArtifactMeta reports as an unknown template.
			return nil, fmt.Errorf("%w: %s@%s", cardtmpl.ErrTemplateUnknown, templateID, activeVersion.String)
		}
		out[cardtmpl.ID(templateID)] = activationPointer{
			activation: activation, version: activeVersion.String,
			source: source.String, columns: columns, blocked: blocked,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("card template catalog: iterate activation pointers: %w", err)
	}
	return out, nil
}

// selectScopedGrantsSQL is loadGrantDecision's query widened to a set of
// templates. It keeps both candidate scopes per template for the same reason
// the single-template form does: an exact tombstone has to be able to shadow an
// active global grant, and a "first match wins" query cannot express that.
const selectScopedGrantsSQL = `SELECT template_id, scope_space_id, status,
        can_discover, can_send, can_edit, revision
    FROM card_template_grant
    WHERE principal_type = ? AND principal_id = ? AND scope_space_id IN (?, '')
      AND template_id IN (`

func scanScopedGrantDecisions(
	ctx context.Context,
	tx *sql.Tx,
	ids []cardtmpl.ID,
	principal cardtmpl.CatalogPrincipal,
) (map[cardtmpl.ID]cardtmpl.RuntimeGrant, error) {
	out := make(map[cardtmpl.ID]cardtmpl.RuntimeGrant, len(ids))
	principalType, principalID, grantable := grantPrincipal(principal)
	if !grantable {
		return out, nil
	}
	scope := strings.TrimSpace(principal.SpaceID)
	query, idArgs := inListQuery(selectScopedGrantsSQL, ids)
	args := append([]any{string(principalType), principalID, scope}, idArgs...)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("card template catalog: load batch authorization grants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	exact := make(map[cardtmpl.ID]GrantRecord, len(ids))
	global := make(map[cardtmpl.ID]GrantRecord, len(ids))
	for rows.Next() {
		var templateID string
		var record GrantRecord
		var status string
		if err := rows.Scan(&templateID, &record.Identity.ScopeSpaceID, &status,
			&record.Permissions.Discover, &record.Permissions.Send, &record.Permissions.Edit,
			&record.Revision); err != nil {
			return nil, fmt.Errorf("card template catalog: scan batch authorization grant: %w", err)
		}
		record.Status = GrantStatus(status)
		if record.Status != GrantStatusActive && record.Status != GrantStatusRevoked {
			return nil, fmt.Errorf("%w: invalid grant status %q", ErrCatalogIntegrity, status)
		}
		id := cardtmpl.ID(templateID)
		record.Identity.TemplateID = id
		record.Identity.PrincipalType, record.Identity.PrincipalID = principalType, principalID
		if record.Identity.ScopeSpaceID == "" {
			global[id] = record
			continue
		}
		if record.Identity.ScopeSpaceID != scope {
			return nil, fmt.Errorf("%w: grant scope %q is outside the query",
				ErrCatalogIntegrity, record.Identity.ScopeSpaceID)
		}
		exact[id] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("card template catalog: iterate batch authorization grants: %w", err)
	}
	for _, id := range ids {
		var exactRow, globalRow *GrantRecord
		if record, ok := exact[id]; ok {
			exactRow = &record
		}
		if record, ok := global[id]; ok {
			globalRow = &record
		}
		out[id] = runtimeGrantFromDecision(resolveGrantRows(exactRow, globalRow))
	}
	return out, nil
}

// inListQuery closes an `... IN (` prefix with one placeholder per id.
func inListQuery(prefix string, ids []cardtmpl.ID) (string, []any) {
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, string(id))
	}
	return prefix + strings.Join(placeholders, ",") + ")", args
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
	grants := make(map[cardtmpl.ID]cardtmpl.RuntimeGrant, len(decisions))
	candidates := make([]cardtmpl.ID, 0, len(decisions))
	for _, id := range sortedTemplateIDs(decisions) {
		grant := runtimeGrantFromDecision(decisions[id])
		if !grant.Discover && !grant.Send && !grant.Edit {
			continue
		}
		grants[id] = grant
		candidates = append(candidates, id)
	}
	// One statement for every candidate rather than two per candidate. The old
	// loop held this REPEATABLE READ snapshot open across up to 129 round trips
	// to assemble a manifest that a Bot polls for feature detection; with a
	// fleet of Bots that is steady primary-DB load for an answer the fleet
	// mostly already knows.
	active, err := loadAdvertisableAuthorizations(ctx, tx, candidates, grants)
	if err != nil {
		return nil, err
	}
	results := make([]cardtmpl.RuntimeAdvertisedTemplate, 0, len(candidates))
	for _, id := range candidates {
		authorization, ok := active[id]
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

// selectActiveAuthorizationsSQL resolves the active pointer and its artifact
// for a bounded set of template IDs in one pass. The inner join on
// active_version does the same filtering the per-template loop did by
// discarding absent and disabled pointers: a disabled row has a NULL
// active_version and joins to nothing, and a template with no activation row
// simply does not appear.
const selectActiveAuthorizationsSQL = `SELECT act.template_id, act.active_version, act.status, act.revision,
        c.source, a.owner, a.visibility, a.engine_contract, a.protocol,
        a.contract_version, a.content_sha256, a.blocked_at IS NOT NULL
    FROM card_template_activation act
    JOIN card_template_version_claim c
      ON c.template_id = act.template_id AND c.version = act.active_version
    LEFT JOIN card_template_artifact a
      ON a.template_id = c.template_id AND a.version = c.version
    WHERE act.template_id IN (`

// loadActiveAuthorizations completes the snapshot for every candidate at once.
// A pointer or artifact that is absent, disabled or malformed is omitted rather
// than reported: the template simply is not advertisable right now, which is a
// different thing from the read having failed. That is the same rule the
// single-template path applies, so the two cannot drift.
func loadAdvertisableAuthorizations(
	ctx context.Context,
	tx *sql.Tx,
	ids []cardtmpl.ID,
	grants map[cardtmpl.ID]cardtmpl.RuntimeGrant,
) (map[cardtmpl.ID]cardtmpl.RuntimeAuthorization, error) {
	out := make(map[cardtmpl.ID]cardtmpl.RuntimeAuthorization, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, string(id))
	}
	rows, err := tx.QueryContext(ctx, selectActiveAuthorizationsSQL+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, fmt.Errorf("card template catalog: load active authorizations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var templateID, status string
		var version sql.NullString
		var revision uint64
		var source string
		var columns artifactMetaColumns
		var blocked bool
		if err := rows.Scan(&templateID, &version, &status, &revision, &source,
			&columns.owner, &columns.visibility, &columns.engine, &columns.protocol,
			&columns.contractVersion, &columns.hash, &blocked); err != nil {
			return nil, fmt.Errorf("card template catalog: scan active authorization: %w", err)
		}
		id := cardtmpl.ID(templateID)
		activation, err := buildRuntimeActivation(version, status, revision)
		if err != nil {
			if errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
				continue
			}
			return nil, err
		}
		activeVersion, err := cardtmpl.ActivationVersion(id, activation)
		if err != nil || activeVersion == "" {
			continue
		}
		meta, err := buildRuntimeArtifactMeta(id, activeVersion, source, columns, blocked)
		if err != nil {
			if errors.Is(err, cardtmpl.ErrTemplateUnknown) || errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
				continue
			}
			return nil, err
		}
		out[id] = cardtmpl.RuntimeAuthorization{
			Activation: activation, Version: activeVersion, Artifact: meta, Grant: grants[id],
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("card template catalog: iterate active authorizations: %w", err)
	}
	return out, nil
}

func scanPrincipalGrants(
	ctx context.Context,
	tx *sql.Tx,
	principalType GrantPrincipalType,
	principalID, scope string,
	rowLimit int,
) (map[cardtmpl.ID]GrantDecision, error) {
	// Read one row past the budget so a truncated result is detectable. A
	// template's exact and global rows are adjacent in this ordering, so a cut
	// can only ever split the *last* template — and half a pair is worse than
	// no pair: losing the exact tombstone of a template whose global row is
	// still active would report a revoked principal as granted.
	rows, err := tx.QueryContext(ctx, selectPrincipalGrantsSQL,
		string(principalType), principalID, scope, rowLimit+1)
	if err != nil {
		return nil, fmt.Errorf("card template catalog: list principal grants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	exact := make(map[cardtmpl.ID]GrantRecord)
	global := make(map[cardtmpl.ID]GrantRecord)
	var order []cardtmpl.ID
	seen := make(map[cardtmpl.ID]struct{})
	truncated := false
	scanned := 0
	for rows.Next() {
		scanned++
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
		if scanned > rowLimit {
			// The peek row. Read its template ID before stopping: it is the only
			// way to tell a cut that landed inside a template from one that
			// landed cleanly on a boundary, and dropping a fully-read template
			// because the *next* one was truncated loses a grant for nothing.
			if len(order) > 0 && order[len(order)-1] == id {
				truncated = true
			}
			break
		}
		record.Identity = GrantIdentity{
			TemplateID: id, PrincipalType: principalType,
			PrincipalID: principalID, ScopeSpaceID: scopeSpaceID,
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			order = append(order, id)
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
	if truncated && len(order) > 0 {
		// The cut landed inside this template, so its row set is partial. Drop
		// it: half a pair is worse than none, because losing an exact tombstone
		// whose global row is still active reports a revoked principal as
		// granted. Every earlier template is complete, since the ordering keeps
		// a template's rows contiguous.
		incomplete := order[len(order)-1]
		delete(exact, incomplete)
		delete(global, incomplete)
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
