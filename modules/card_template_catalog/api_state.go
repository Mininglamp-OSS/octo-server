package card_template_catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"go.uber.org/zap"
)

const stateControlTimeout = 10 * time.Second

var (
	managerTemplateIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9][a-z0-9-]*)+$`)
	managerVersionPattern    = regexp.MustCompile(
		`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
			`(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
			`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`,
	)
)

type catalogStateStore interface {
	Activate(context.Context, ActivationRequest) (ActivationResult, error)
	Rollback(context.Context, RollbackRequest) (ActivationResult, error)
	Block(context.Context, BlockRequest) (BlockResult, error)
	GetTemplateDetail(context.Context, cardtmpl.ID, int) (TemplateDetail, error)
	ListAudit(context.Context, cardtmpl.ID, uint64, int) (TemplateAuditPage, error)
}

type activateControlRequest struct {
	Version          string `json:"version"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Reason           string `json:"reason"`
	ChangeTicket     string `json:"change_ticket"`
}

type rollbackControlRequest struct {
	TargetVersion    string `json:"target_version"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Reason           string `json:"reason"`
	ChangeTicket     string `json:"change_ticket"`
}

type blockControlRequest struct {
	ExpectedRevision uint64 `json:"expected_revision"`
	FallbackVersion  string `json:"fallback_version,omitempty"`
	Reason           string `json:"reason"`
	ChangeTicket     string `json:"change_ticket"`
}

type activationControlResponse struct {
	TemplateID       string `json:"template_id"`
	PreviousVersion  string `json:"previous_version,omitempty"`
	ActiveVersion    string `json:"active_version,omitempty"`
	Status           string `json:"status"`
	PreviousRevision uint64 `json:"previous_revision"`
	Revision         uint64 `json:"revision"`
}

type blockControlResponse struct {
	TemplateID       string `json:"template_id"`
	Version          string `json:"version"`
	Blocked          bool   `json:"blocked"`
	PreviousVersion  string `json:"previous_version,omitempty"`
	ActiveVersion    string `json:"active_version,omitempty"`
	Status           string `json:"status,omitempty"`
	PreviousRevision uint64 `json:"previous_revision"`
	Revision         uint64 `json:"revision"`
}

func (a *API) activate(c *wkhttp.Context) {
	resultLabel := "rejected"
	defer func() { a.metrics.observeOperation("activate", resultLabel) }()
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
	id, ok := parseManagerTemplateID(c.Param("id"))
	if !ok {
		respondCatalogRequestInvalid(c, ErrPublishRequestInvalid)
		return
	}
	var request activateControlRequest
	if !readStrictStateRequest(c, &request, "version", "expected_revision", "reason", "change_ticket") {
		return
	}
	if !validManagerVersion(request.Version) || !validStateControlText(request.Reason, request.ChangeTicket) {
		respondCatalogRequestInvalid(c, ErrPublishRequestInvalid)
		return
	}
	if a.validateStateTarget == nil {
		respondCatalogUnavailable(c)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), stateControlTimeout)
	defer cancel()
	receipt, err := a.validateStateTarget(ctx, id, request.Version)
	if err != nil {
		resultLabel = a.respondStateError(c, "activate_validate", err)
		return
	}
	store, ok := a.stateStore(c)
	if !ok {
		return
	}
	result, err := store.Activate(ctx, ActivationRequest{
		TemplateID: id, TargetVersion: request.Version, ExpectedRevision: request.ExpectedRevision,
		ActorUID: c.GetLoginUID(), Reason: request.Reason, ChangeTicket: request.ChangeTicket,
		targetReceipt: receipt,
	})
	if err != nil {
		resultLabel = a.respondStateError(c, "activate", err)
		return
	}
	resultLabel = "ok"
	a.metrics.observeDB("activate", "ok")
	c.Response(activationResponse(result))
}

func (a *API) rollback(c *wkhttp.Context) {
	resultLabel := "rejected"
	defer func() { a.metrics.observeOperation("rollback", resultLabel) }()
	if !a.requireSuperAdmin(c) {
		return
	}
	if ready, result := a.requireRuntimeReady(c); !ready {
		resultLabel = result
		return
	}
	id, ok := parseManagerTemplateID(c.Param("id"))
	if !ok {
		respondCatalogRequestInvalid(c, ErrPublishRequestInvalid)
		return
	}
	var request rollbackControlRequest
	if !readStrictStateRequest(c, &request, "target_version", "expected_revision", "reason", "change_ticket") {
		return
	}
	if !validManagerVersion(request.TargetVersion) || !validStateControlText(request.Reason, request.ChangeTicket) {
		respondCatalogRequestInvalid(c, ErrPublishRequestInvalid)
		return
	}
	if a.validateStateTarget == nil {
		respondCatalogUnavailable(c)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), stateControlTimeout)
	defer cancel()
	receipt, err := a.validateStateTarget(ctx, id, request.TargetVersion)
	if err != nil {
		resultLabel = a.respondStateError(c, "rollback_validate", err)
		return
	}
	store, ok := a.stateStore(c)
	if !ok {
		return
	}
	result, err := store.Rollback(ctx, RollbackRequest{
		TemplateID: id, TargetVersion: request.TargetVersion, ExpectedRevision: request.ExpectedRevision,
		ActorUID: c.GetLoginUID(), Reason: request.Reason, ChangeTicket: request.ChangeTicket,
		targetReceipt: receipt,
	})
	if err != nil {
		resultLabel = a.respondStateError(c, "rollback", err)
		return
	}
	resultLabel = "ok"
	a.metrics.observeDB("rollback", "ok")
	c.Response(activationResponse(result))
}

func (a *API) block(c *wkhttp.Context) {
	resultLabel := "rejected"
	defer func() { a.metrics.observeOperation("block", resultLabel) }()
	if !a.requireSuperAdmin(c) {
		return
	}
	if ready, result := a.requireRuntimeReady(c); !ready {
		resultLabel = result
		return
	}
	id, version, ok := parseManagerIdentity(c.Param("id"))
	if !ok {
		respondCatalogRequestInvalid(c, ErrPublishRequestInvalid)
		return
	}
	var request blockControlRequest
	if !readStrictStateRequest(c, &request, "expected_revision", "reason", "change_ticket") {
		return
	}
	if (request.FallbackVersion != "" && !validManagerVersion(request.FallbackVersion)) ||
		!validStateControlText(request.Reason, request.ChangeTicket) {
		respondCatalogRequestInvalid(c, ErrPublishRequestInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), stateControlTimeout)
	defer cancel()
	var fallbackReceipt stateTargetReceipt
	if request.FallbackVersion != "" {
		if a.validateStateTarget == nil {
			respondCatalogUnavailable(c)
			return
		}
		var err error
		fallbackReceipt, err = a.validateStateTarget(ctx, id, request.FallbackVersion)
		if err != nil {
			resultLabel = a.respondStateError(c, "block_fallback_validate", err)
			return
		}
	}
	store, ok := a.stateStore(c)
	if !ok {
		return
	}
	result, err := store.Block(ctx, BlockRequest{
		TemplateID: id, Version: version, ExpectedRevision: request.ExpectedRevision,
		FallbackVersion: request.FallbackVersion, ActorUID: c.GetLoginUID(),
		Reason: request.Reason, ChangeTicket: request.ChangeTicket,
		fallbackReceipt: fallbackReceipt,
	})
	if err != nil {
		resultLabel = a.respondStateError(c, "block", err)
		return
	}
	resultLabel = "ok"
	a.metrics.observeDB("block", "ok")
	c.Response(blockControlResponse{
		TemplateID: string(result.TemplateID), Version: result.Version, Blocked: result.Blocked,
		PreviousVersion: result.PreviousVersion, ActiveVersion: result.ActiveVersion, Status: string(result.Status),
		PreviousRevision: result.PreviousRevision, Revision: result.Revision,
	})
}

func (a *API) detail(c *wkhttp.Context) {
	if !a.requireSuperAdmin(c) {
		return
	}
	id, ok := parseManagerTemplateID(c.Param("id"))
	if !ok {
		respondCatalogRequestInvalid(c, ErrPublishRequestInvalid)
		return
	}
	store, ok := a.stateStore(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), stateControlTimeout)
	defer cancel()
	detail, err := store.GetTemplateDetail(ctx, id, managerReadHardLimit)
	if err != nil {
		a.respondStateError(c, "detail", err)
		return
	}
	response := templateDetailResponse(detail)
	// Bounded grant summary（PR-C）：grant store 可用时附带；fake/legacy store
	// 不实现该接口则省略（响应 grants 恒为数组，不因缺失变形）。
	if grantStore, ok := a.store.(catalogGrantStore); ok && grantStore != nil {
		grants, truncated, grantsErr := grantStore.ListGrants(ctx, id, grantSummaryLimit)
		if grantsErr != nil {
			// The grant summary is an add-on to a control-plane read that
			// worked long before grants existed. Failing the whole response
			// would take activation state, versions and revisions away from an
			// operator at exactly the moment they need them — including the
			// case where the grant migration has not been applied yet. Report
			// the outage in the logs and serve the rest.
			// Still record the DB outcome. Degrading the response must not also
			// degrade observability: without this the detail_grants metric never
			// fires for any outcome, and a persistent grant-store outage reads
			// as 100% success on the dashboard that exists to catch it.
			a.metrics.observeDB("detail_grants", "error")
			a.logStateError("card template detail grant summary unavailable", "detail_grants", grantsErr)
			response.GrantsUnavailable = true
			c.Response(response)
			return
		}
		// The matching success observation. Without it the error counter above
		// is the only sample the detail_grants metric ever takes, so the
		// error-rate panel it exists to feed reads 100% the moment it has any
		// data at all — the exact inverse of the blind spot it was added for.
		a.metrics.observeDB("detail_grants", "ok")
		response.Grants = make([]grantSummaryResponse, len(grants))
		for i, record := range grants {
			response.Grants[i] = grantSummaryResponse{
				PrincipalType: string(record.Identity.PrincipalType),
				PrincipalID:   record.Identity.PrincipalID,
				ScopeSpaceID:  record.Identity.ScopeSpaceID,
				Status:        string(record.Status),
				Discover:      record.Permissions.Discover,
				Send:          record.Permissions.Send,
				Edit:          record.Permissions.Edit,
				Revision:      record.Revision,
				UpdatedBy:     record.UpdatedBy,
			}
		}
		response.GrantsTruncated = truncated
	}
	c.Response(response)
}

func (a *API) audit(c *wkhttp.Context) {
	if !a.requireSuperAdmin(c) {
		return
	}
	id, ok := parseManagerTemplateID(c.Param("id"))
	if !ok {
		respondCatalogRequestInvalid(c, ErrPublishRequestInvalid)
		return
	}
	limit, err := parseBoundedUintQuery(c.Query("limit"), managerReadDefaultLimit, managerReadHardLimit)
	if err != nil {
		respondCatalogRequestInvalid(c, err)
		return
	}
	cursor, err := parseBoundedUintQuery(c.Query("cursor"), 0, int(^uint(0)>>1))
	if err != nil {
		respondCatalogRequestInvalid(c, err)
		return
	}
	store, ok := a.stateStore(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), stateControlTimeout)
	defer cancel()
	page, err := store.ListAudit(ctx, id, uint64(cursor), limit)
	if err != nil {
		a.respondStateError(c, "audit", err)
		return
	}
	c.Response(templateAuditResponse(page))
}

func (a *API) stateStore(c *wkhttp.Context) (catalogStateStore, bool) {
	store, ok := a.store.(catalogStateStore)
	if !ok || store == nil {
		respondCatalogUnavailable(c)
		return nil, false
	}
	return store, true
}

func (a *API) respondStateError(c *wkhttp.Context, operation string, err error) string {
	switch {
	case errors.Is(err, ErrActivationConflict), errors.Is(err, ErrActiveTargetCapacity):
		a.metrics.observeDB(operation, "conflict")
		respondCatalogStateConflict(c)
		return "conflict"
	case errors.Is(err, ErrArtifactBlocked):
		a.metrics.observeDB(operation, "blocked")
		respondCatalogBlocked(c)
		return "blocked"
	case errors.Is(err, ErrActivationTargetInvalid), errors.Is(err, ErrBlockTargetInvalid),
		errors.Is(err, ErrRollbackTargetInvalid), errors.Is(err, ErrPublishRequestInvalid),
		errors.Is(err, cardtmpl.ErrTemplateUnknown):
		a.metrics.observeDB(operation, "rejected")
		respondCatalogRequestInvalid(c, err)
		return "rejected"
	case errors.Is(err, ErrCatalogIntegrity), errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity):
		a.metrics.observeDB(operation, "integrity_error")
		a.logStateError("card template catalog integrity failure", operation, err)
		respondCatalogIntegrityFailure(c)
		return "integrity_error"
	case errors.Is(err, context.Canceled):
		a.metrics.observeDB(operation, "canceled")
		respondCatalogUnavailable(c)
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, cardtmpl.ErrArtifactCompileBusy):
		a.metrics.observeDB(operation, "timeout")
		respondCatalogUnavailable(c)
		return "timeout"
	default:
		a.metrics.observeDB(operation, "error")
		a.logStateError("card template catalog state operation failed", operation, err)
		respondCatalogUnavailable(c)
		return "error"
	}
}

func (a *API) logStateError(message, operation string, err error) {
	if a.logger != nil {
		a.logger.Error(message, zap.String("operation", operation), zap.Error(err))
	}
}

func readStrictStateRequest(c *wkhttp.Context, out any, required ...string) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, controlBodyMaxBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			respondCatalogContentTooLarge(c)
		} else {
			respondCatalogRequestInvalid(c, err)
		}
		return false
	}
	root, err := cardtmpl.DecodeStrictJSONObject(raw)
	if err != nil {
		respondCatalogRequestInvalid(c, err)
		return false
	}
	for key, value := range root {
		if value == nil {
			respondCatalogRequestInvalid(c, fmt.Errorf("request field %q must not be null", key))
			return false
		}
	}
	for _, key := range required {
		value, exists := root[key]
		if !exists || value == nil {
			respondCatalogRequestInvalid(c, fmt.Errorf("request field %q is required", key))
			return false
		}
	}
	if err := cardtmpl.DecodeStrictJSON(raw, out); err != nil {
		respondCatalogRequestInvalid(c, err)
		return false
	}
	return true
}

func parseManagerTemplateID(raw string) (cardtmpl.ID, bool) {
	return cardtmpl.ID(raw), len(raw) <= 128 && managerTemplateIDPattern.MatchString(raw)
}

func parseManagerIdentity(raw string) (cardtmpl.ID, string, bool) {
	index := strings.LastIndexByte(raw, '@')
	if index <= 0 || index == len(raw)-1 {
		return "", "", false
	}
	id, ok := parseManagerTemplateID(raw[:index])
	version := raw[index+1:]
	return id, version, ok && validManagerVersion(version)
}

func validManagerVersion(version string) bool {
	return len(version) <= 64 && managerVersionPattern.MatchString(version)
}

func validStateControlText(reason, ticket string) bool {
	return boundedRequiredText(reason, maxReasonRunes) && boundedRequiredText(ticket, maxChangeTicketRunes)
}

func parseBoundedUintQuery(raw string, fallback, hardMax int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseUint(raw, 10, 63)
	if err != nil {
		return 0, errors.New("invalid unsigned query value")
	}
	if value > uint64(hardMax) {
		value = uint64(hardMax)
	}
	return int(value), nil
}

func activationResponse(result ActivationResult) activationControlResponse {
	return activationControlResponse{
		TemplateID: string(result.TemplateID), PreviousVersion: result.PreviousVersion,
		ActiveVersion: result.ActiveVersion, Status: string(result.Status),
		PreviousRevision: result.PreviousRevision, Revision: result.Revision,
	}
}

type activationReadResponse struct {
	Exists   bool   `json:"exists"`
	Version  string `json:"version,omitempty"`
	Status   string `json:"status,omitempty"`
	Revision uint64 `json:"revision"`
}

type versionReadResponse struct {
	Version         string                 `json:"version"`
	Source          cardtmpl.RuntimeSource `json:"source"`
	Owner           string                 `json:"owner,omitempty"`
	Visibility      string                 `json:"visibility,omitempty"`
	Engine          string                 `json:"engine,omitempty"`
	Protocol        string                 `json:"protocol,omitempty"`
	ContractVersion string                 `json:"contract_version,omitempty"`
	Hash            string                 `json:"content_sha256,omitempty"`
	Blocked         bool                   `json:"blocked"`
	BlockedAt       *time.Time             `json:"blocked_at,omitempty"`
	CreatedAt       time.Time              `json:"created_at"`
}

type detailReadResponse struct {
	TemplateID string                 `json:"template_id"`
	Activation activationReadResponse `json:"activation"`
	Versions   []versionReadResponse  `json:"versions"`
	Truncated  bool                   `json:"truncated"`
	// Grants 是 bounded grant summary（PR-C Control APIs）：只在 superAdmin
	// manager detail 出现，普通 B1/B2 永不返回这些 control fields。
	Grants []grantSummaryResponse `json:"grants"`
	// GrantsUnavailable marks a response whose grant summary could not be read.
	// It is distinct from an empty Grants array — "this template has no grants"
	// and "we could not tell" are different answers for an operator.
	GrantsUnavailable bool `json:"grants_unavailable,omitempty"`
	GrantsTruncated   bool `json:"grants_truncated,omitempty"`
}

type grantSummaryResponse struct {
	PrincipalType string `json:"principal_type"`
	PrincipalID   string `json:"principal_id"`
	ScopeSpaceID  string `json:"scope_space_id"`
	Status        string `json:"status"`
	Discover      bool   `json:"discover"`
	Send          bool   `json:"send"`
	Edit          bool   `json:"edit"`
	Revision      uint64 `json:"revision"`
	UpdatedBy     string `json:"updated_by"`
}

func templateDetailResponse(detail TemplateDetail) detailReadResponse {
	response := detailReadResponse{
		TemplateID: detail.TemplateID, Grants: []grantSummaryResponse{},
		Activation: activationReadResponse{
			Exists: detail.Activation.Exists, Version: detail.Activation.Version,
			Status: string(detail.Activation.Status), Revision: detail.Activation.Revision,
		},
		Versions: make([]versionReadResponse, len(detail.Versions)), Truncated: detail.Truncated,
	}
	for i, version := range detail.Versions {
		response.Versions[i] = versionReadResponse{
			Version: version.Version, Source: version.Source, Owner: version.Owner,
			Visibility: version.Visibility, Engine: version.Engine, Protocol: version.Protocol,
			ContractVersion: version.ContractVersion, Hash: version.Hash, Blocked: version.Blocked,
			BlockedAt: version.BlockedAt, CreatedAt: version.CreatedAt,
		}
	}
	return response
}

type auditReadResponse struct {
	Entries    []TemplateAuditEntry `json:"entries"`
	NextCursor uint64               `json:"next_cursor,omitempty"`
}

func templateAuditResponse(page TemplateAuditPage) auditReadResponse {
	if page.Entries == nil {
		page.Entries = []TemplateAuditEntry{}
	}
	return auditReadResponse{Entries: page.Entries, NextCursor: page.NextCursor}
}
