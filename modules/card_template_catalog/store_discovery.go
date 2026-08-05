package card_template_catalog

// PR-C D5 — the discovery read model behind B1/B2.
//
// The load-bearing property here is an ordering one: visibility, block state
// and the caller's discover grant are applied *inside* the SQL predicate, so
// they are settled before LIMIT runs. The obvious alternative — fetch a page,
// then drop the rows the caller may not see — silently returns short pages,
// lets a hidden row consume a slot, and turns `has_more` into a lie. Worse, the
// count of dropped rows is itself an enumeration oracle: a caller could infer
// how many private templates exist by watching pages shrink.
//
// A `space` principal is the caller's Space acting on its own behalf. Its grant
// rows are always global-scoped (D2 forbids a Space-scoped grant *to* a Space,
// which would be a scope of a scope), so the lookup below matches on the
// canonical empty sentinel rather than going through the two-row precedence
// reducer that producer principals need.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

const (
	// defaultDiscoveryPageSize / maxDiscoveryPageSize are the D5 page bounds.
	defaultDiscoveryPageSize = 50
	maxDiscoveryPageSize     = 100
	// maxStaticGrantProbe bounds the IN-list used to check discover grants on
	// static template IDs. The static Registry is a code-reviewed, small set;
	// a deployment past this bound has outgrown a single-page listing anyway.
	maxStaticGrantProbe = 128
)

const (
	// selectDiscoverableSQL is one row per visible, unblocked dynamic exact
	// version. The row-value cursor comparison keeps the scan on the primary
	// key order, and the grant join is an existence test rather than a fetch —
	// nothing about the grant reaches the response.
	selectDiscoverableSQL = `SELECT c.template_id, c.version, a.owner, a.protocol,
            a.contract_version, a.visibility, a.content_sha256,
            act.active_version IS NOT NULL AND act.active_version = c.version
        FROM card_template_version_claim c
        JOIN card_template_artifact a
          ON a.template_id = c.template_id AND a.version = c.version
        LEFT JOIN card_template_activation act
          ON act.template_id = c.template_id AND act.status = 'active'
        LEFT JOIN card_template_grant g
          ON g.template_id = c.template_id AND g.principal_type = 'space'
         AND g.principal_id = ? AND g.scope_space_id = ''
         AND g.status = 'active' AND g.can_discover = 1
        WHERE c.source = 'dynamic'
          AND a.blocked_at IS NULL
          AND (a.visibility = ? OR g.template_id IS NOT NULL)
          AND (c.template_id, c.version) > (?, ?)
        ORDER BY c.template_id ASC, c.version ASC
        LIMIT ?`
	// selectDiscoverableExactSQL is the B2 metadata read. It applies the same
	// predicate, so an invisible, blocked or unknown template is a single
	// indistinguishable empty result — the caller cannot tell which.
	selectDiscoverableExactSQL = `SELECT c.template_id, c.version, a.owner, a.protocol,
            a.contract_version, a.visibility, a.content_sha256,
            act.active_version IS NOT NULL AND act.active_version = c.version
        FROM card_template_version_claim c
        JOIN card_template_artifact a
          ON a.template_id = c.template_id AND a.version = c.version
        LEFT JOIN card_template_activation act
          ON act.template_id = c.template_id AND act.status = 'active'
        LEFT JOIN card_template_grant g
          ON g.template_id = c.template_id AND g.principal_type = 'space'
         AND g.principal_id = ? AND g.scope_space_id = ''
         AND g.status = 'active' AND g.can_discover = 1
        WHERE c.template_id = ? AND c.version = ?
          AND c.source = 'dynamic'
          AND a.blocked_at IS NULL
          AND (a.visibility = ? OR g.template_id IS NOT NULL)`
)

// ErrDiscoveryNotVisible is the single outcome for "no such template", "you may
// not see it" and "it is blocked". Callers must not widen it: three distinct
// answers would let anyone map the private catalog by probing IDs.
var ErrDiscoveryNotVisible = fmt.Errorf("card template is not discoverable")

// DiscoveryRow is one visible exact version as B1 reports it. It carries no
// grant, audit or storage state — only the contract surface a producer needs
// to decide whether to look the template up in full.
type DiscoveryRow struct {
	ID               cardtmpl.ID
	Version          string
	Owner            string
	Protocol         string
	ContractVersion  string
	Visibility       string
	ContentSHA256    string
	ActiveForNewSend bool
}

// ListDiscoverable returns one page of visible dynamic versions after the
// cursor position, plus whether more remain.
func (s *store) ListDiscoverable(
	ctx context.Context,
	spaceID string,
	afterID cardtmpl.ID,
	afterVersion string,
	limit int,
) ([]DiscoveryRow, bool, error) {
	limit = boundedDiscoveryLimit(limit)
	rows, err := s.db.QueryContext(ctx, selectDiscoverableSQL,
		strings.TrimSpace(spaceID), cardtmpl.CatalogVisibilityPublic,
		string(afterID), afterVersion, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("card template catalog: list discoverable: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := make([]DiscoveryRow, 0, limit)
	for rows.Next() {
		row, err := scanDiscoveryRow(rows)
		if err != nil {
			return nil, false, err
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("card template catalog: iterate discoverable: %w", err)
	}
	if len(page) > limit {
		return page[:limit], true, nil
	}
	return page, false, nil
}

// LoadDiscoverable answers B2's metadata half. ErrDiscoveryNotVisible covers
// every reason the row is absent.
func (s *store) LoadDiscoverable(
	ctx context.Context,
	spaceID string,
	id cardtmpl.ID,
	version string,
) (DiscoveryRow, error) {
	rows, err := s.db.QueryContext(ctx, selectDiscoverableExactSQL,
		strings.TrimSpace(spaceID), string(id), version, cardtmpl.CatalogVisibilityPublic)
	if err != nil {
		return DiscoveryRow{}, fmt.Errorf("card template catalog: load discoverable: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return DiscoveryRow{}, fmt.Errorf("card template catalog: load discoverable: %w", err)
		}
		return DiscoveryRow{}, ErrDiscoveryNotVisible
	}
	row, err := scanDiscoveryRow(rows)
	if err != nil {
		return DiscoveryRow{}, err
	}
	return row, nil
}

// StaticDiscoverGrants reports which of the given template IDs this Space holds
// an active discover grant on. Static templates are private by default, so
// without this a granted static card would be invisible even to the Space it
// was granted to.
func (s *store) StaticDiscoverGrants(
	ctx context.Context,
	spaceID string,
	ids []cardtmpl.ID,
) (map[cardtmpl.ID]struct{}, error) {
	granted := make(map[cardtmpl.ID]struct{})
	space := strings.TrimSpace(spaceID)
	if space == "" || len(ids) == 0 {
		return granted, nil
	}
	if len(ids) > maxStaticGrantProbe {
		ids = ids[:maxStaticGrantProbe]
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, space)
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, string(id))
	}
	query := `SELECT template_id FROM card_template_grant
        WHERE principal_type = 'space' AND principal_id = ? AND scope_space_id = ''
          AND status = 'active' AND can_discover = 1
          AND template_id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("card template catalog: static discover grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("card template catalog: scan static discover grant: %w", err)
		}
		granted[cardtmpl.ID(id)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("card template catalog: iterate static discover grants: %w", err)
	}
	return granted, nil
}

func scanDiscoveryRow(rows *sql.Rows) (DiscoveryRow, error) {
	var row DiscoveryRow
	var templateID string
	var owner, protocol, contractVersion, visibility, hash sql.NullString
	if err := rows.Scan(&templateID, &row.Version, &owner, &protocol,
		&contractVersion, &visibility, &hash, &row.ActiveForNewSend); err != nil {
		return DiscoveryRow{}, fmt.Errorf("card template catalog: scan discoverable: %w", err)
	}
	row.ID = cardtmpl.ID(templateID)
	row.Owner, row.Protocol = owner.String, protocol.String
	row.ContractVersion, row.ContentSHA256 = contractVersion.String, hash.String
	row.Visibility = cardtmpl.CatalogVisibilityPrivate
	if visibility.String == cardtmpl.CatalogVisibilityPublic {
		row.Visibility = cardtmpl.CatalogVisibilityPublic
	}
	return row, nil
}

func boundedDiscoveryLimit(limit int) int {
	if limit <= 0 {
		return defaultDiscoveryPageSize
	}
	if limit > maxDiscoveryPageSize {
		return maxDiscoveryPageSize
	}
	return limit
}
