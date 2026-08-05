package card_template_catalog

// PR-C D2/D4（spec: .octospec/tasks/cardtmpl-runtime-catalog-grants-discovery/
// brief.md）：template-ID 级 grant 的持久化与 CAS。
//
//   - identity = (template_id, principal_type, principal_id, scope_space_id)
//     unique；global scope 用 canonical 空串 sentinel；
//   - create 要求 expected_revision=0；update/revoke 要求精确当前 revision，
//     并发只有一个赢家；revoke 写 tombstone（权限清零 + revision 递增），
//     绝不物理删除 —— revoke→recreate 拿不回旧 revision（防 ABA）；
//   - grant target 只要求「至少存在一个 permanent version claim」（任意
//     source），用同事务 SELECT ... FOR UPDATE 验证，不造跨 static/dynamic
//     的脆弱 FK；
//   - 状态写与 append-only audit 同事务提交（audit 记 principal/scope 与
//     before/after permissions/revision，不存 token/卡片 data）；
//   - 精确覆盖顺序（exact-over-global、tombstone 屏蔽）collapse 在唯一的
//     resolveGrantRows 上，Slice 3 的 one-snapshot authorizer 复用同一函数，
//     不允许第二处实现。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

type GrantPrincipalType string

const (
	GrantPrincipalBot              GrantPrincipalType = "bot"
	GrantPrincipalInternalProducer GrantPrincipalType = "internal_producer"
	GrantPrincipalSpace            GrantPrincipalType = "space"
)

type GrantStatus string

const (
	GrantStatusActive  GrantStatus = "active"
	GrantStatusRevoked GrantStatus = "revoked"

	auditOperationGrant  = "grant"
	auditOperationRevoke = "revoke"

	maxGrantPrincipalIDRunes = 128
	maxGrantScopeSpaceRunes  = 128
)

var (
	ErrGrantRequestInvalid = errors.New("card template grant request invalid")
	ErrGrantConflict       = errors.New("card template grant revision conflict")
	ErrGrantTargetUnknown  = errors.New("card template grant target has no version claim")
	ErrGrantNotFound       = errors.New("card template grant does not exist")
	ErrGrantAlreadyRevoked = errors.New("card template grant is already revoked")
)

type GrantPermissions struct {
	Discover bool
	Send     bool
	Edit     bool
}

type GrantIdentity struct {
	TemplateID    cardtmpl.ID
	PrincipalType GrantPrincipalType
	PrincipalID   string
	// ScopeSpaceID 的 canonical 空串表示 global scope。
	ScopeSpaceID string
}

type GrantRecord struct {
	Identity     GrantIdentity
	Status       GrantStatus
	Permissions  GrantPermissions
	Revision     uint64
	UpdatedBy    string
	Reason       string
	ChangeTicket string
	UpdatedAt    time.Time
}

type UpsertGrantRequest struct {
	Identity         GrantIdentity
	Permissions      GrantPermissions
	ExpectedRevision uint64
	ActorUID         string
	Reason           string
	ChangeTicket     string
}

type RevokeGrantRequest struct {
	Identity         GrantIdentity
	ExpectedRevision uint64
	ActorUID         string
	Reason           string
	ChangeTicket     string
}

type GrantResult struct {
	Record  GrantRecord
	Created bool
}

// GrantDecision 是 resolveGrantRows 的输出：生效权限 + 命中的 scope 来源
// （"exact" / "global" / ""），供 receipt/日志与授权判定使用。
type GrantDecision struct {
	Allowed     GrantPermissions
	ScopeSource string
	Revision    uint64
}

const (
	selectGrantForUpdateSQL = `SELECT status, can_discover, can_send, can_edit, revision
        FROM card_template_grant
        WHERE template_id = ? AND principal_type = ? AND principal_id = ? AND scope_space_id = ?
        FOR UPDATE`
	selectAnyClaimForUpdateSQL = `SELECT version FROM card_template_version_claim
        WHERE template_id = ?
        LIMIT 1
        FOR UPDATE`
	insertGrantSQL = `INSERT INTO card_template_grant
        (template_id, principal_type, principal_id, scope_space_id, status,
         can_discover, can_send, can_edit, revision, updated_by, reason, change_ticket)
        VALUES (?, ?, ?, ?, 'active', ?, ?, ?, 1, ?, ?, ?)`
	updateGrantSQL = `UPDATE card_template_grant
        SET status = ?, can_discover = ?, can_send = ?, can_edit = ?,
            revision = revision + 1, updated_by = ?, reason = ?, change_ticket = ?,
            updated_at = CURRENT_TIMESTAMP(6)
        WHERE template_id = ? AND principal_type = ? AND principal_id = ? AND scope_space_id = ?
          AND revision = ?`
	insertGrantAuditSQL = `INSERT INTO card_template_audit
        (actor_uid, operation, template_id, version, previous_version, resulting_version,
         previous_revision, resulting_revision, principal_type, principal_id, scope_space_id,
         previous_permissions, resulting_permissions, content_sha256, result, reason, change_ticket)
        VALUES (?, ?, ?, '', '', '', ?, ?, ?, ?, ?, ?, ?, '', 'ok', ?, ?)`
	selectGrantsByTemplateSQL = `SELECT principal_type, principal_id, scope_space_id, status,
         can_discover, can_send, can_edit, revision, updated_by, reason, change_ticket, updated_at
        FROM card_template_grant
        WHERE template_id = ?
        ORDER BY principal_type, principal_id, scope_space_id
        LIMIT ?`
)

func (s *store) UpsertGrant(ctx context.Context, request UpsertGrantRequest) (GrantResult, error) {
	if err := validateUpsertGrantRequest(request); err != nil {
		return GrantResult{}, err
	}
	return executeStateWithRetry(ctx, func() (GrantResult, error) {
		return s.upsertGrantOnce(ctx, request)
	})
}

func (s *store) upsertGrantOnce(ctx context.Context, request UpsertGrantRequest) (GrantResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GrantResult{}, fmt.Errorf("card template catalog: begin grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := requireAnyVersionClaim(ctx, tx, request.Identity.TemplateID); err != nil {
		return GrantResult{}, err
	}
	current, exists, err := loadGrantForUpdate(ctx, tx, request.Identity)
	if err != nil {
		return GrantResult{}, err
	}
	if !exists {
		if request.ExpectedRevision != 0 {
			return GrantResult{}, fmt.Errorf("%w: expected %d, grant does not exist",
				ErrGrantConflict, request.ExpectedRevision)
		}
		if _, err := tx.ExecContext(ctx, insertGrantSQL,
			string(request.Identity.TemplateID), string(request.Identity.PrincipalType),
			request.Identity.PrincipalID, request.Identity.ScopeSpaceID,
			request.Permissions.Discover, request.Permissions.Send, request.Permissions.Edit,
			request.ActorUID, request.Reason, request.ChangeTicket,
		); err != nil {
			if isDuplicateKey(err) {
				return GrantResult{}, fmt.Errorf("%w: concurrent first grant", ErrGrantConflict)
			}
			return GrantResult{}, fmt.Errorf("card template catalog: insert grant: %w", err)
		}
	} else {
		// update / re-activate-tombstone 都要求携带当前精确 revision；对
		// revoked row 不能再用 expected_revision=0 伪装 create（D4 反 ABA）。
		if request.ExpectedRevision != current.Revision {
			return GrantResult{}, fmt.Errorf("%w: expected %d, current %d",
				ErrGrantConflict, request.ExpectedRevision, current.Revision)
		}
		result, err := tx.ExecContext(ctx, updateGrantSQL,
			GrantStatusActive, request.Permissions.Discover, request.Permissions.Send, request.Permissions.Edit,
			request.ActorUID, request.Reason, request.ChangeTicket,
			string(request.Identity.TemplateID), string(request.Identity.PrincipalType),
			request.Identity.PrincipalID, request.Identity.ScopeSpaceID, request.ExpectedRevision,
		)
		if err != nil {
			return GrantResult{}, fmt.Errorf("card template catalog: update grant: %w", err)
		}
		if err := requireOneStateRow(result); err != nil {
			return GrantResult{}, err
		}
	}
	nextRevision := current.Revision + 1
	if err := insertGrantAudit(ctx, tx, grantAudit{
		actorUID: request.ActorUID, operation: auditOperationGrant,
		identity:          request.Identity,
		previousRevision:  current.Revision,
		resultingRevision: nextRevision,
		previousPerms:     current.Permissions,
		previousStatus:    current.Status,
		previousExists:    exists,
		resultingPerms:    request.Permissions,
		reason:            request.Reason,
		changeTicket:      request.ChangeTicket,
	}); err != nil {
		return GrantResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return GrantResult{}, fmt.Errorf("card template catalog: commit grant: %w", err)
	}
	return GrantResult{
		Record: GrantRecord{
			Identity: request.Identity, Status: GrantStatusActive,
			Permissions: request.Permissions, Revision: nextRevision,
			UpdatedBy: request.ActorUID, Reason: request.Reason, ChangeTicket: request.ChangeTicket,
		},
		Created: !exists,
	}, nil
}

func (s *store) RevokeGrant(ctx context.Context, request RevokeGrantRequest) (GrantResult, error) {
	if err := validateRevokeGrantRequest(request); err != nil {
		return GrantResult{}, err
	}
	return executeStateWithRetry(ctx, func() (GrantResult, error) {
		return s.revokeGrantOnce(ctx, request)
	})
}

func (s *store) revokeGrantOnce(ctx context.Context, request RevokeGrantRequest) (GrantResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GrantResult{}, fmt.Errorf("card template catalog: begin revoke: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, exists, err := loadGrantForUpdate(ctx, tx, request.Identity)
	if err != nil {
		return GrantResult{}, err
	}
	if !exists {
		return GrantResult{}, fmt.Errorf("%w: %s/%s", ErrGrantNotFound,
			request.Identity.PrincipalType, request.Identity.PrincipalID)
	}
	if current.Status == GrantStatusRevoked {
		return GrantResult{}, fmt.Errorf("%w: revision %d", ErrGrantAlreadyRevoked, current.Revision)
	}
	if request.ExpectedRevision != current.Revision {
		return GrantResult{}, fmt.Errorf("%w: expected %d, current %d",
			ErrGrantConflict, request.ExpectedRevision, current.Revision)
	}
	result, err := tx.ExecContext(ctx, updateGrantSQL,
		GrantStatusRevoked, false, false, false,
		request.ActorUID, request.Reason, request.ChangeTicket,
		string(request.Identity.TemplateID), string(request.Identity.PrincipalType),
		request.Identity.PrincipalID, request.Identity.ScopeSpaceID, request.ExpectedRevision,
	)
	if err != nil {
		return GrantResult{}, fmt.Errorf("card template catalog: revoke grant: %w", err)
	}
	if err := requireOneStateRow(result); err != nil {
		return GrantResult{}, err
	}
	nextRevision := current.Revision + 1
	if err := insertGrantAudit(ctx, tx, grantAudit{
		actorUID: request.ActorUID, operation: auditOperationRevoke,
		identity:          request.Identity,
		previousRevision:  current.Revision,
		resultingRevision: nextRevision,
		previousPerms:     current.Permissions,
		previousStatus:    current.Status,
		previousExists:    true,
		resultingPerms:    GrantPermissions{},
		reason:            request.Reason,
		changeTicket:      request.ChangeTicket,
	}); err != nil {
		return GrantResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return GrantResult{}, fmt.Errorf("card template catalog: commit revoke: %w", err)
	}
	return GrantResult{
		Record: GrantRecord{
			Identity: request.Identity, Status: GrantStatusRevoked,
			Revision: nextRevision, UpdatedBy: request.ActorUID,
			Reason: request.Reason, ChangeTicket: request.ChangeTicket,
		},
	}, nil
}

// ListGrants 返回 template 的 bounded grant 清单（manager detail 摘要用；
// 普通 B1/B2 永不透出）。truncated=true 表示还有更多行未返回。
func (s *store) ListGrants(ctx context.Context, id cardtmpl.ID, limit int) ([]GrantRecord, bool, error) {
	if strings.TrimSpace(string(id)) == "" || limit < 1 {
		return nil, false, fmt.Errorf("%w: list requires template and positive limit", ErrGrantRequestInvalid)
	}
	rows, err := s.db.QueryContext(ctx, selectGrantsByTemplateSQL, string(id), limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("card template catalog: list grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]GrantRecord, 0, limit)
	for rows.Next() {
		record := GrantRecord{Identity: GrantIdentity{TemplateID: id}}
		var principalType, status string
		if err := rows.Scan(&principalType, &record.Identity.PrincipalID, &record.Identity.ScopeSpaceID,
			&status, &record.Permissions.Discover, &record.Permissions.Send, &record.Permissions.Edit,
			&record.Revision, &record.UpdatedBy, &record.Reason, &record.ChangeTicket, &record.UpdatedAt,
		); err != nil {
			return nil, false, fmt.Errorf("card template catalog: scan grant: %w", err)
		}
		record.Identity.PrincipalType = GrantPrincipalType(principalType)
		record.Status = GrantStatus(status)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("card template catalog: iterate grants: %w", err)
	}
	if len(records) > limit {
		return records[:limit], true, nil
	}
	return records, false, nil
}

// resolveGrantRows 是 exact-over-global 覆盖顺序的唯一实现（D2）：
// exact row 存在（active 或 tombstone）就完全遮蔽 global row —— active 按其
// permission 判定、tombstone 直接拒绝；只有 exact row 不存在才看 global。
// 权限绝不 union。
func resolveGrantRows(exact, global *GrantRecord) GrantDecision {
	row := exact
	source := "exact"
	if row == nil {
		row = global
		source = "global"
	}
	if row == nil {
		return GrantDecision{}
	}
	decision := GrantDecision{ScopeSource: source, Revision: row.Revision}
	if row.Status == GrantStatusActive {
		decision.Allowed = row.Permissions
	}
	return decision
}

func validateGrantIdentity(identity GrantIdentity) error {
	if strings.TrimSpace(string(identity.TemplateID)) == "" ||
		string(identity.TemplateID) != strings.TrimSpace(string(identity.TemplateID)) {
		return fmt.Errorf("%w: template id is required", ErrGrantRequestInvalid)
	}
	if !boundedRequiredText(identity.PrincipalID, maxGrantPrincipalIDRunes) ||
		identity.PrincipalID != strings.TrimSpace(identity.PrincipalID) {
		return fmt.Errorf("%w: principal id is required and bounded", ErrGrantRequestInvalid)
	}
	if identity.ScopeSpaceID != strings.TrimSpace(identity.ScopeSpaceID) ||
		len([]rune(identity.ScopeSpaceID)) > maxGrantScopeSpaceRunes {
		return fmt.Errorf("%w: scope space id must be trimmed and bounded", ErrGrantRequestInvalid)
	}
	switch identity.PrincipalType {
	case GrantPrincipalBot, GrantPrincipalInternalProducer:
		return nil
	case GrantPrincipalSpace:
		// D2 canonical shape：space principal 只有 principal_id=<space_id> +
		// global sentinel scope 一种形状，杜绝 (principal space A, scope
		// space B) 的跨租户组合。
		if identity.ScopeSpaceID != "" {
			return fmt.Errorf("%w: space principal must use the global scope sentinel", ErrGrantRequestInvalid)
		}
		return nil
	default:
		return fmt.Errorf("%w: principal type %q is not grantable", ErrGrantRequestInvalid, identity.PrincipalType)
	}
}

func validateGrantPermissions(principalType GrantPrincipalType, permissions GrantPermissions) error {
	// D2：active grant 至少一项权限，且 send/edit 必须伴随 discover（否则是
	// capability/discovery 观察不到的暗授权）—— 合并后等价于必须包含 discover。
	if !permissions.Discover {
		return fmt.Errorf("%w: an active grant must include discover", ErrGrantRequestInvalid)
	}
	if principalType == GrantPrincipalSpace && (permissions.Send || permissions.Edit) {
		return fmt.Errorf("%w: a space principal is discover-only", ErrGrantRequestInvalid)
	}
	return nil
}

func validateUpsertGrantRequest(request UpsertGrantRequest) error {
	if err := validateGrantIdentity(request.Identity); err != nil {
		return err
	}
	if err := validateGrantPermissions(request.Identity.PrincipalType, request.Permissions); err != nil {
		return err
	}
	return validateGrantControlText(request.ActorUID, request.Reason, request.ChangeTicket)
}

func validateRevokeGrantRequest(request RevokeGrantRequest) error {
	if err := validateGrantIdentity(request.Identity); err != nil {
		return err
	}
	return validateGrantControlText(request.ActorUID, request.Reason, request.ChangeTicket)
}

func validateGrantControlText(actorUID, reason, changeTicket string) error {
	if !boundedRequiredText(actorUID, maxActorUIDRunes) ||
		!boundedRequiredText(reason, maxReasonRunes) ||
		!boundedRequiredText(changeTicket, maxChangeTicketRunes) {
		return fmt.Errorf("%w: actor, reason and change ticket are required and bounded", ErrGrantRequestInvalid)
	}
	return nil
}

// requireAnyVersionClaim 用同事务锁定验证 grant target 至少有一个 permanent
// version claim（任意 source；不以 activation 存在为前提，D4）。
func requireAnyVersionClaim(ctx context.Context, tx *sql.Tx, id cardtmpl.ID) error {
	var version string
	err := tx.QueryRowContext(ctx, selectAnyClaimForUpdateSQL, string(id)).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrGrantTargetUnknown, id)
	}
	if err != nil {
		return fmt.Errorf("card template catalog: lock grant target claim: %w", err)
	}
	return nil
}

func loadGrantForUpdate(ctx context.Context, tx *sql.Tx, identity GrantIdentity) (GrantRecord, bool, error) {
	record := GrantRecord{Identity: identity}
	var status string
	err := tx.QueryRowContext(ctx, selectGrantForUpdateSQL,
		string(identity.TemplateID), string(identity.PrincipalType),
		identity.PrincipalID, identity.ScopeSpaceID,
	).Scan(&status, &record.Permissions.Discover, &record.Permissions.Send,
		&record.Permissions.Edit, &record.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return GrantRecord{Identity: identity}, false, nil
	}
	if err != nil {
		return GrantRecord{}, false, fmt.Errorf("card template catalog: load grant: %w", err)
	}
	record.Status = GrantStatus(status)
	if record.Status != GrantStatusActive && record.Status != GrantStatusRevoked {
		return GrantRecord{}, false, fmt.Errorf("%w: invalid grant status %q", ErrCatalogIntegrity, status)
	}
	return record, true, nil
}

type grantAudit struct {
	actorUID          string
	operation         string
	identity          GrantIdentity
	previousRevision  uint64
	resultingRevision uint64
	previousPerms     GrantPermissions
	previousStatus    GrantStatus
	previousExists    bool
	resultingPerms    GrantPermissions
	reason            string
	changeTicket      string
}

func insertGrantAudit(ctx context.Context, tx *sql.Tx, audit grantAudit) error {
	previousLabel := ""
	if audit.previousExists && audit.previousStatus == GrantStatusActive {
		previousLabel = grantPermissionsLabel(audit.previousPerms)
	}
	resultingLabel := ""
	if audit.operation == auditOperationGrant {
		resultingLabel = grantPermissionsLabel(audit.resultingPerms)
	}
	if _, err := tx.ExecContext(ctx, insertGrantAuditSQL,
		audit.actorUID, audit.operation, string(audit.identity.TemplateID),
		audit.previousRevision, audit.resultingRevision,
		string(audit.identity.PrincipalType), audit.identity.PrincipalID, audit.identity.ScopeSpaceID,
		previousLabel, resultingLabel, audit.reason, audit.changeTicket,
	); err != nil {
		return fmt.Errorf("card template catalog: insert %s audit: %w", audit.operation, err)
	}
	return nil
}

// grantPermissionsLabel 输出 audit 用的 canonical 权限标签（固定顺序）。
func grantPermissionsLabel(permissions GrantPermissions) string {
	parts := make([]string, 0, 3)
	if permissions.Discover {
		parts = append(parts, "discover")
	}
	if permissions.Send {
		parts = append(parts, "send")
	}
	if permissions.Edit {
		parts = append(parts, "edit")
	}
	return strings.Join(parts, ",")
}
