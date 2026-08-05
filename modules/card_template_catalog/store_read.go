package card_template_catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

const (
	managerReadDefaultLimit = 50
	managerReadHardLimit    = 100

	selectTemplateVersionsSQL = `SELECT c.version, c.source, a.owner, a.visibility, a.engine_contract,
        a.protocol, a.contract_version, a.content_sha256, a.blocked_at, c.created_at
        FROM card_template_version_claim c
        LEFT JOIN card_template_artifact a
          ON a.template_id = c.template_id AND a.version = c.version
        WHERE c.template_id = ?
        ORDER BY c.version ASC
        LIMIT ?`
	selectTemplateAuditSQL = `SELECT id, actor_uid, operation, version, previous_version, resulting_version,
        previous_revision, resulting_revision, principal_type, principal_id, scope_space_id,
        previous_permissions, resulting_permissions, result, reason, change_ticket, created_at
        FROM card_template_audit
        WHERE template_id = ? AND (? = 0 OR id < ?)
        ORDER BY id DESC
        LIMIT ?`
)

type TemplateVersionDetail struct {
	Version         string
	Source          cardtmpl.RuntimeSource
	Owner           string
	Visibility      string
	Engine          string
	Protocol        string
	ContractVersion string
	Hash            string
	Blocked         bool
	BlockedAt       *time.Time
	CreatedAt       time.Time
}

type TemplateDetail struct {
	TemplateID string
	Activation cardtmpl.RuntimeActivation
	Versions   []TemplateVersionDetail
	Truncated  bool
}

type TemplateAuditEntry struct {
	ID                uint64  `json:"id"`
	ActorUID          string  `json:"actor_uid"`
	Operation         string  `json:"operation"`
	Version           string  `json:"version,omitempty"`
	PreviousVersion   string  `json:"previous_version,omitempty"`
	ResultingVersion  string  `json:"resulting_version,omitempty"`
	PreviousRevision  *uint64 `json:"previous_revision,omitempty"`
	ResultingRevision *uint64 `json:"resulting_revision,omitempty"`
	// PR-C grant/revoke 审计的脱敏 principal 身份与权限差异（无 token、无
	// 卡片 data）；非 grant 操作行恒为空串。
	PrincipalType        string    `json:"principal_type,omitempty"`
	PrincipalID          string    `json:"principal_id,omitempty"`
	ScopeSpaceID         string    `json:"scope_space_id,omitempty"`
	PreviousPermissions  string    `json:"previous_permissions,omitempty"`
	ResultingPermissions string    `json:"resulting_permissions,omitempty"`
	Result               string    `json:"result"`
	Reason               string    `json:"reason"`
	ChangeTicket         string    `json:"change_ticket"`
	CreatedAt            time.Time `json:"created_at"`
}

type TemplateAuditPage struct {
	Entries    []TemplateAuditEntry
	NextCursor uint64
}

func (s *store) GetTemplateDetail(ctx context.Context, id cardtmpl.ID, limit int) (TemplateDetail, error) {
	limit = normalizeManagerReadLimit(limit)
	activation, err := s.LoadActivation(ctx, id)
	if err != nil {
		return TemplateDetail{}, err
	}
	rows, err := s.db.QueryContext(ctx, selectTemplateVersionsSQL, string(id), limit+1)
	if err != nil {
		return TemplateDetail{}, fmt.Errorf("card template catalog: list template versions: %w", err)
	}
	defer rows.Close()

	versions := make([]TemplateVersionDetail, 0, limit)
	for rows.Next() {
		var version, source string
		var owner, visibility, engine, protocol, contractVersion, hash sql.NullString
		var blockedAt sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&version, &source, &owner, &visibility, &engine, &protocol,
			&contractVersion, &hash, &blockedAt, &createdAt); err != nil {
			return TemplateDetail{}, fmt.Errorf("card template catalog: scan template version: %w", err)
		}
		detail := TemplateVersionDetail{Version: version, CreatedAt: createdAt}
		switch source {
		case claimSourceStatic:
			detail.Source = cardtmpl.RuntimeSourceStatic
			if owner.Valid || visibility.Valid || engine.Valid || protocol.Valid || contractVersion.Valid || hash.Valid || blockedAt.Valid {
				return TemplateDetail{}, fmt.Errorf("%w: static claim has artifact metadata", ErrCatalogIntegrity)
			}
		case claimSourceDynamic:
			if !owner.Valid || !visibility.Valid || !engine.Valid || !protocol.Valid || !contractVersion.Valid || !hash.Valid {
				return TemplateDetail{}, fmt.Errorf("%w: dynamic claim has incomplete artifact metadata", ErrCatalogIntegrity)
			}
			detail.Source = cardtmpl.RuntimeSourceDynamic
			detail.Owner, detail.Visibility, detail.Engine = owner.String, visibility.String, engine.String
			detail.Protocol, detail.ContractVersion, detail.Hash = protocol.String, contractVersion.String, hash.String
			detail.Blocked = blockedAt.Valid
			if blockedAt.Valid {
				value := blockedAt.Time
				detail.BlockedAt = &value
			}
		default:
			return TemplateDetail{}, fmt.Errorf("%w: unknown claim source", ErrCatalogIntegrity)
		}
		versions = append(versions, detail)
	}
	if err := rows.Err(); err != nil {
		return TemplateDetail{}, fmt.Errorf("card template catalog: iterate template versions: %w", err)
	}
	truncated := len(versions) > limit
	if truncated {
		versions = versions[:limit]
	}
	return TemplateDetail{
		TemplateID: string(id), Activation: activation, Versions: versions, Truncated: truncated,
	}, nil
}

func (s *store) ListAudit(
	ctx context.Context,
	id cardtmpl.ID,
	cursor uint64,
	limit int,
) (TemplateAuditPage, error) {
	limit = normalizeManagerReadLimit(limit)
	rows, err := s.db.QueryContext(ctx, selectTemplateAuditSQL, string(id), cursor, cursor, limit+1)
	if err != nil {
		return TemplateAuditPage{}, fmt.Errorf("card template catalog: list audit: %w", err)
	}
	defer rows.Close()
	entries := make([]TemplateAuditEntry, 0, limit)
	for rows.Next() {
		var entry TemplateAuditEntry
		var previousRevision, resultingRevision sql.NullInt64
		if err := rows.Scan(
			&entry.ID, &entry.ActorUID, &entry.Operation, &entry.Version,
			&entry.PreviousVersion, &entry.ResultingVersion,
			&previousRevision, &resultingRevision,
			&entry.PrincipalType, &entry.PrincipalID, &entry.ScopeSpaceID,
			&entry.PreviousPermissions, &entry.ResultingPermissions,
			&entry.Result, &entry.Reason,
			&entry.ChangeTicket, &entry.CreatedAt,
		); err != nil {
			return TemplateAuditPage{}, fmt.Errorf("card template catalog: scan audit: %w", err)
		}
		if previousRevision.Valid {
			if previousRevision.Int64 < 0 {
				return TemplateAuditPage{}, fmt.Errorf("%w: negative previous revision", ErrCatalogIntegrity)
			}
			value := uint64(previousRevision.Int64)
			entry.PreviousRevision = &value
		}
		if resultingRevision.Valid {
			if resultingRevision.Int64 < 0 {
				return TemplateAuditPage{}, fmt.Errorf("%w: negative resulting revision", ErrCatalogIntegrity)
			}
			value := uint64(resultingRevision.Int64)
			entry.ResultingRevision = &value
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return TemplateAuditPage{}, fmt.Errorf("card template catalog: iterate audit: %w", err)
	}
	page := TemplateAuditPage{Entries: entries}
	if len(entries) > limit {
		page.Entries = entries[:limit]
		page.NextCursor = page.Entries[len(page.Entries)-1].ID
	}
	return page, nil
}

func normalizeManagerReadLimit(limit int) int {
	if limit <= 0 {
		return managerReadDefaultLimit
	}
	if limit > managerReadHardLimit {
		return managerReadHardLimit
	}
	return limit
}
