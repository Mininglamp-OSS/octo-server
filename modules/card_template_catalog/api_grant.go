package card_template_catalog

// PR-C D2/D4 Control APIs（spec: .octospec/tasks/
// cardtmpl-runtime-catalog-grants-discovery/brief.md）：superAdmin manager
// grant 写接口。
//
//   PUT    /v1/manager/card-templates/{id}/grants/{principal_type}/{principal_id}
//   DELETE /v1/manager/card-templates/{id}/grants/{principal_type}/{principal_id}
//
// 两条都要求 control gate=true、runtime ready、superAdmin、strict body、
// 10s deadline；actor 只取认证上下文（body 内 actor 字段随 strict decode
// 被拒）；principal/scope 先过 D2 canonical 校验与存在性校验（bot 活跃、
// internal producer 已注册、Space 存在且 active），再进 store CAS。响应回
// 实际命中的 resulting revision，manager 据此做下一次 CAS。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

// catalogGrantStore 是 grant 写/读的窄接口；生产 *store 实现，测试注入 fake。
type catalogGrantStore interface {
	UpsertGrant(context.Context, UpsertGrantRequest) (GrantResult, error)
	RevokeGrant(context.Context, RevokeGrantRequest) (GrantResult, error)
	ListGrants(context.Context, cardtmpl.ID, int) ([]GrantRecord, bool, error)
}

// grantSummaryLimit bounds the manager-detail grant summary (D5: control
// fields never reach ordinary B1/B2; the full list pages through audit).
const grantSummaryLimit = 32

type grantUpsertControlRequest struct {
	ScopeSpaceID     string `json:"scope_space_id"`
	Discover         bool   `json:"discover"`
	Send             bool   `json:"send"`
	Edit             bool   `json:"edit"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Reason           string `json:"reason"`
	ChangeTicket     string `json:"change_ticket"`
}

type grantRevokeControlRequest struct {
	ScopeSpaceID     string `json:"scope_space_id"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Reason           string `json:"reason"`
	ChangeTicket     string `json:"change_ticket"`
}

type grantControlResponse struct {
	TemplateID    string `json:"template_id"`
	PrincipalType string `json:"principal_type"`
	PrincipalID   string `json:"principal_id"`
	ScopeSpaceID  string `json:"scope_space_id"`
	Status        string `json:"status"`
	Discover      bool   `json:"discover"`
	Send          bool   `json:"send"`
	Edit          bool   `json:"edit"`
	Revision      uint64 `json:"revision"`
	Created       bool   `json:"created,omitempty"`
}

func (a *API) grantUpsert(c *wkhttp.Context) {
	resultLabel := "rejected"
	defer func() { a.metrics.observeOperation("grant", resultLabel) }()
	if !a.requireSuperAdmin(c) {
		return
	}
	if !a.controlEnabled {
		resultLabel = "disabled"
		respondCatalogControlDisabled(c)
		return
	}
	if ready, result := a.requireRuntimeReady(c); !ready {
		resultLabel = result
		return
	}
	identity, ok := a.parseGrantPath(c)
	if !ok {
		return
	}
	var request grantUpsertControlRequest
	if !readStrictStateRequest(c, &request,
		"scope_space_id", "discover", "send", "edit", "expected_revision", "reason", "change_ticket") {
		return
	}
	identity.ScopeSpaceID = request.ScopeSpaceID
	permissions := GrantPermissions{Discover: request.Discover, Send: request.Send, Edit: request.Edit}
	if err := validateGrantIdentity(identity); err != nil {
		a.respondCatalogGrantInvalid(c, err)
		return
	}
	if err := validateGrantPermissions(identity.PrincipalType, permissions); err != nil {
		a.respondCatalogGrantInvalid(c, err)
		return
	}
	if !validStateControlText(request.Reason, request.ChangeTicket) {
		a.respondCatalogGrantInvalid(c, ErrGrantRequestInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), stateControlTimeout)
	defer cancel()
	if !a.requireGrantPrincipal(c, ctx, identity) {
		return
	}
	store, ok := a.grantStore(c)
	if !ok {
		return
	}
	result, err := store.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity, Permissions: permissions,
		ExpectedRevision: request.ExpectedRevision,
		ActorUID:         c.GetLoginUID(),
		Reason:           request.Reason, ChangeTicket: request.ChangeTicket,
	})
	if err != nil {
		resultLabel = a.respondGrantError(c, "grant", err)
		return
	}
	resultLabel = "ok"
	a.metrics.observeDB("grant", "ok")
	c.Response(grantResponse(result))
}

func (a *API) grantRevoke(c *wkhttp.Context) {
	resultLabel := "rejected"
	defer func() { a.metrics.observeOperation("revoke", resultLabel) }()
	if !a.requireSuperAdmin(c) {
		return
	}
	if !a.controlEnabled {
		resultLabel = "disabled"
		respondCatalogControlDisabled(c)
		return
	}
	if ready, result := a.requireRuntimeReady(c); !ready {
		resultLabel = result
		return
	}
	identity, ok := a.parseGrantPath(c)
	if !ok {
		return
	}
	var request grantRevokeControlRequest
	if !readStrictStateRequest(c, &request,
		"scope_space_id", "expected_revision", "reason", "change_ticket") {
		return
	}
	identity.ScopeSpaceID = request.ScopeSpaceID
	if err := validateGrantIdentity(identity); err != nil {
		a.respondCatalogGrantInvalid(c, err)
		return
	}
	if !validStateControlText(request.Reason, request.ChangeTicket) {
		a.respondCatalogGrantInvalid(c, ErrGrantRequestInvalid)
		return
	}
	// Revoke 不做 principal 存在性校验：principal 事后被删除/停用时，撤销
	// 其残留 grant 恰恰是必须可行的运维操作。
	ctx, cancel := context.WithTimeout(c.Request.Context(), stateControlTimeout)
	defer cancel()
	store, ok := a.grantStore(c)
	if !ok {
		return
	}
	result, err := store.RevokeGrant(ctx, RevokeGrantRequest{
		Identity:         identity,
		ExpectedRevision: request.ExpectedRevision,
		ActorUID:         c.GetLoginUID(),
		Reason:           request.Reason, ChangeTicket: request.ChangeTicket,
	})
	if err != nil {
		resultLabel = a.respondGrantError(c, "revoke", err)
		return
	}
	resultLabel = "ok"
	a.metrics.observeDB("revoke", "ok")
	c.Response(grantResponse(result))
}

func (a *API) parseGrantPath(c *wkhttp.Context) (GrantIdentity, bool) {
	id, ok := parseManagerTemplateID(c.Param("id"))
	if !ok {
		a.respondCatalogGrantInvalid(c, ErrGrantRequestInvalid)
		return GrantIdentity{}, false
	}
	principalType := GrantPrincipalType(c.Param("principal_type"))
	switch principalType {
	case GrantPrincipalBot, GrantPrincipalInternalProducer, GrantPrincipalSpace:
	default:
		a.respondCatalogGrantInvalid(c, fmt.Errorf("%w: principal type %q", ErrGrantRequestInvalid, principalType))
		return GrantIdentity{}, false
	}
	principalID := c.Param("principal_id")
	if principalID == "" || principalID != strings.TrimSpace(principalID) ||
		len([]rune(principalID)) > maxGrantPrincipalIDRunes {
		a.respondCatalogGrantInvalid(c, ErrGrantRequestInvalid)
		return GrantIdentity{}, false
	}
	return GrantIdentity{TemplateID: id, PrincipalType: principalType, PrincipalID: principalID}, true
}

// requireGrantPrincipal 校验 principal（与非空 scope Space）真实存在且可认证
// （D2）。validator 未接线是组合根 bug —— fail-close 到 unavailable，绝不放行。
func (a *API) requireGrantPrincipal(c *wkhttp.Context, ctx context.Context, identity GrantIdentity) bool {
	if a.validateGrantPrincipal == nil {
		a.logStateError("card template grant principal validator is not wired", "grant", nil)
		respondCatalogUnavailable(c)
		return false
	}
	if err := a.validateGrantPrincipal(ctx, identity); err != nil {
		if errors.Is(err, ErrGrantRequestInvalid) {
			a.respondCatalogGrantInvalid(c, err)
			return false
		}
		a.logStateError("card template grant principal validation failed", "grant", err)
		respondCatalogUnavailable(c)
		return false
	}
	return true
}

func (a *API) grantStore(c *wkhttp.Context) (catalogGrantStore, bool) {
	store, ok := a.store.(catalogGrantStore)
	if !ok || store == nil {
		respondCatalogUnavailable(c)
		return nil, false
	}
	return store, true
}

func (a *API) respondGrantError(c *wkhttp.Context, operation string, err error) string {
	switch {
	case errors.Is(err, ErrGrantConflict), errors.Is(err, ErrGrantAlreadyRevoked):
		a.metrics.observeDB(operation, "conflict")
		respondCatalogStateConflict(c)
		return "conflict"
	case errors.Is(err, ErrGrantRequestInvalid), errors.Is(err, ErrGrantTargetUnknown),
		errors.Is(err, ErrGrantNotFound):
		a.metrics.observeDB(operation, "rejected")
		a.respondCatalogGrantInvalid(c, err)
		return "rejected"
	case errors.Is(err, ErrCatalogIntegrity):
		a.metrics.observeDB(operation, "integrity_error")
		a.logStateError("card template catalog integrity failure", operation, err)
		respondCatalogIntegrityFailure(c)
		return "integrity_error"
	case errors.Is(err, context.Canceled):
		a.metrics.observeDB(operation, "canceled")
		respondCatalogUnavailable(c)
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		a.metrics.observeDB(operation, "timeout")
		respondCatalogUnavailable(c)
		return "timeout"
	default:
		a.metrics.observeDB(operation, "error")
		a.logStateError("card template grant operation failed", operation, err)
		respondCatalogUnavailable(c)
		return "error"
	}
}

func grantResponse(result GrantResult) grantControlResponse {
	record := result.Record
	return grantControlResponse{
		TemplateID:    string(record.Identity.TemplateID),
		PrincipalType: string(record.Identity.PrincipalType),
		PrincipalID:   record.Identity.PrincipalID,
		ScopeSpaceID:  record.Identity.ScopeSpaceID,
		Status:        string(record.Status),
		Discover:      record.Permissions.Discover,
		Send:          record.Permissions.Send,
		Edit:          record.Permissions.Edit,
		Revision:      record.Revision,
		Created:       result.Created,
	}
}

// productionGrantPrincipalCheck 是组合根接线的存在性校验实现：
//   - bot：botidentity（robot/app-bot 表）活跃身份；
//   - internal_producer：carddispatch 注册表的只读身份绑定；
//   - space principal 与非空 scope Space：space 表存在且 status=1。
func (a *API) productionGrantPrincipalCheck(ctx context.Context, identity GrantIdentity) error {
	switch identity.PrincipalType {
	case GrantPrincipalBot:
		active, err := botidentity.New(a.ctx).Active(identity.PrincipalID)
		if err != nil {
			return fmt.Errorf("card template catalog: resolve bot principal: %w", err)
		}
		if !active {
			return fmt.Errorf("%w: bot principal is not an active bot", ErrGrantRequestInvalid)
		}
		if err := a.requireByteExactBot(ctx, identity.PrincipalID); err != nil {
			return err
		}
	case GrantPrincipalInternalProducer:
		if _, ok := carddispatch.ProducerBindingFromContext(a.ctx, carddispatch.ProducerID(identity.PrincipalID)); !ok {
			return fmt.Errorf("%w: internal producer is not registered", ErrGrantRequestInvalid)
		}
	case GrantPrincipalSpace:
		if err := a.requireActiveSpace(ctx, identity.PrincipalID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: principal type %q", ErrGrantRequestInvalid, identity.PrincipalType)
	}
	if identity.ScopeSpaceID != "" {
		return a.requireActiveSpace(ctx, identity.ScopeSpaceID)
	}
	return nil
}

// Existence checks here run against tables collated utf8mb4_general_ci, while
// card_template_grant.principal_id / scope_space_id are utf8mb4_bin and the
// runtime resolver matches them byte-for-byte. Those two facts together made a
// case-differing identity write successfully and authorize nothing: a
// `PUT .../grants/space/SPACE-1` against a real `space-1` passed the
// general_ci existence check, stored `SPACE-1`, returned 200 with a revision,
// and then never matched a single runtime lookup (review P2, yujiawei).
//
// Fail-closed, but expensive to diagnose — the operator sees a successful write
// and a capability that does nothing, with nothing anywhere connecting the two.
// So the write path rejects a spelling the read path could not match, and names
// the canonical one, which is safe to disclose on a super-admin-only route.
//
// Rejecting rather than silently canonicalising: the stored identity is part of
// the grant's CAS/revision contract, and quietly writing a different identity
// than the caller asked for is its own surprise.
func (a *API) requireActiveSpace(ctx context.Context, spaceID string) error {
	var stored string
	err := a.ctx.DB().DB.QueryRowContext(ctx,
		"SELECT space_id FROM space WHERE space_id = ? AND status = 1 LIMIT 1", spaceID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: space does not exist or is not active", ErrGrantRequestInvalid)
	}
	if err != nil {
		return fmt.Errorf("card template catalog: resolve space: %w", err)
	}
	return byteExactIdentity("space id", stored, spaceID)
}

// byteExactIdentity compares a caller-supplied identity against the canonical
// one. Split out from its two call sites so the rule is testable without a
// live server context, which the production principal check needs and the
// package's tests do not have.
func byteExactIdentity(what, stored, requested string) error {
	if stored == requested {
		return nil
	}
	return fmt.Errorf("%w: %s must match the stored identity exactly (%q)",
		ErrGrantRequestInvalid, what, stored)
}

// requireByteExactBot is the same guard for Bot principals. botidentity.Active
// answers through robot / app_bot, both utf8mb4_general_ci, so it accepts a
// spelling the grant table's utf8mb4_bin lookup will never match. The canonical
// id is re-read here rather than by changing botidentity, whose other callers
// resolve identities for delivery and want the forgiving comparison.
func (a *API) requireByteExactBot(ctx context.Context, botID string) error {
	var stored string
	err := a.ctx.DB().DB.QueryRowContext(ctx,
		"SELECT COALESCE("+
			"(SELECT robot_id FROM robot WHERE robot_id = ? AND status = 1 LIMIT 1),"+
			"(SELECT uid FROM app_bot WHERE uid = ? AND status = 1 LIMIT 1), '')",
		botID, botID).Scan(&stored)
	if err != nil {
		return fmt.Errorf("card template catalog: resolve bot identity: %w", err)
	}
	if stored == "" {
		// Active() already said this Bot exists, so an empty answer here means
		// the two reads disagree — treat it as the same invalid request rather
		// than writing a grant nothing will match.
		return fmt.Errorf("%w: bot principal is not an active bot", ErrGrantRequestInvalid)
	}
	return byteExactIdentity("bot id", stored, botID)
}
