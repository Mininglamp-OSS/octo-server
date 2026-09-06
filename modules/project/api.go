package project

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

// Project is the modules/project API.
type Project struct {
	ctx *config.Context
	log.Log
	db  *DB
	cfg Config
	// settings resolves the feature switch at REQUEST time rather than at
	// construction. cfg carries the quota/cadence knobs, which are genuinely
	// process-scoped; the on/off switch is not — see requireWriteEnabled.
	//
	// Nil only in tests that build a bare struct; requireWriteEnabled falls back
	// to cfg.CreateEnabled in that case so no test has to know about this field.
	settings *common.SystemSettings
	// spaceCache is pkg/space's own membership cache, deliberately reused rather
	// than reimplemented — see projectMemberCacheKey for why a second copy of that
	// fact under a project: key would be an isolation hole.
	spaceCache *spacepkg.RedisMembershipCache
	// auditSink is nil in production (entries go to the structured log). Tests set it so
	// the "every write path audits" contract is assertable without capturing the
	// process-wide logger.
	auditSink auditSink

	// addOneFn / removeOneFn are the per-target execution seams for the batch endpoints.
	//
	// Fields on the instance, not package-level vars: the earlier form put a mutable,
	// not-thread-safe function pointer named ForTest on the production authorization path
	// (yujiawei Q7, PR #841 round 1). New installs the real implementations; a test that
	// needs mid-batch behavior swaps the field on ITS OWN instance (mounted via
	// mountProject) and restores it, so nothing global is rewritable in production.
	addOneFn    func(projectID, spaceID, actorUID, uid string) (bool, error)
	removeOneFn func(projectID, spaceID, actorUID, targetUID string) (bool, error)

	// updateFn / disbandFn are the execution seams for the two single-shot write handlers.
	//
	// They exist because those handlers' ERROR paths turned out to need behavioural coverage
	// and had none: the actor-Space-seat arm added in round 3 fell out of its switch without a
	// return, so control resumed on the SUCCESS path — one handler panicked on a nil model, the
	// other wrote an audit entry claiming a disband that had been refused. A source guard
	// asserting the arm merely EXISTS cannot see that (PR #841 round 4, P1-1/P1-2).
	//
	// In steady state the state that reaches those arms is unreachable from the wire:
	// projectMiddleware resolves Space membership with a live uncached read and refuses first,
	// so only the middleware-to-transaction race window produces it. A seam is the only way to
	// drive it deterministically.
	updateFn  func(projectID, actorUID, spaceID string, req updateReq) (*Model, error)
	disbandFn func(projectID, actorUID, spaceID string) ([]string, error)

	// i1PageFn is the page-query seam for the I1 reconcile scan, on the same terms as the
	// two above.
	//
	// One seam, not four: all four scans share the identical page loop, and what needs a
	// behavioural test is that a MID-SCAN page failure keeps the progress made so far —
	// which cannot be provoked from the outside, because it needs page 1 to succeed and
	// page 2 to fail. The other three scans are held to the same shape by a source guard
	// (TestReconcileScansKeepProgressOnAPageError) rather than by three more fields.
	i1PageFn func(cursorProject, cursorUID string, limit int) ([]*i1Row, error)
}

// New builds the Project API and registers the Space-removal cascade step.
//
// The cascade is registered here, at construction, rather than in Route(): the very
// first thing createProject does is write an owner seat, so octo_project_member has
// active rows from the moment this module is loaded, and I1's reverse direction has
// to already exist by then.
func New(ctx *config.Context) *Project {
	p := &Project{
		ctx: ctx,
		Log: log.NewTLog("Project"),
		db:  NewDB(ctx),
		cfg: loadConfig(),
		// EnsureSystemSettings returns the process-wide instance with its
		// auto-reload goroutine already running, so an admin-console flip
		// converges here within the reload interval without a restart.
		settings: common.EnsureSystemSettings(ctx),
	}
	// nil-conn deployments (Redis-less mode) leave spaceCache nil so the middleware degrades
	// to the database instead of dereferencing a nil redis.Conn. The other Redis paths already
	// check GetRedisConn() per call.
	if ctx.GetRedisConn() != nil {
		p.spaceCache = spacepkg.NewRedisMembershipCache(ctx.GetRedisConn())
	}
	// The batch seams are instance fields (see the struct comment): production gets the real
	// implementations here, and tests swap them on their own instance.
	p.addOneFn = func(projectID, spaceID, actorUID, uid string) (bool, error) {
		return p.addOneMember(projectID, spaceID, actorUID, uid)
	}
	p.removeOneFn = func(projectID, spaceID, actorUID, targetUID string) (bool, error) {
		return p.removeMember(projectID, spaceID, actorUID, targetUID)
	}
	p.updateFn = func(projectID, actorUID, spaceID string, req updateReq) (*Model, error) {
		return p.updateProject(projectID, actorUID, spaceID, req)
	}
	p.disbandFn = func(projectID, actorUID, spaceID string) ([]string, error) {
		return p.disbandProject(projectID, actorUID, spaceID)
	}
	p.i1PageFn = func(cursorProject, cursorUID string, limit int) ([]*i1Row, error) {
		return p.queryI1ViolationPage(cursorProject, cursorUID, limit)
	}

	p.registerSpaceMemberRemovalCleanup()
	return p
}

// Route mounts the Project endpoints.
//
// Two groups, and the middleware order within each is load-bearing:
//
//   - AuthMiddleware FIRST, then SharedUIDRateLimiter. Mounted the other way round
//     the limiter cannot read the uid and silently fails open, i.e. the route looks
//     rate-limited and is not. A "429 under load" test would pass either way, which
//     is why the ordering is asserted structurally in api_i18n_test.go instead.
//   - then the Space/Project resolver, which is what actually confines the request
//     to a tenant. pkg/space's SpaceMiddleware is NOT usable here: it reads only the
//     `space_id` query parameter and the X-Space-ID header and PASSES when neither is
//     present, so a path-parameter route mounted under it would be unguarded.
//
// P0 mounts no unauthenticated route, so StrictIPRateLimitMiddleware has no place
// here; it arrives with the anonymous invite-preview endpoint (P2).
//
// These routes are deliberately NOT contributed to any pkg/authtree tree — see the
// note added to that package's census.
func (p *Project) Route(r *wkhttp.WKHttp) {
	p.startReconcileWorker()

	spaceScoped := r.Group("/v1/space",
		p.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, p.ctx),
		p.spaceIDParamMiddleware(),
	)
	{
		spaceScoped.POST("/:space_id/projects", p.createProjectHandler)
		spaceScoped.GET("/:space_id/projects", p.listProjectsHandler)
	}

	projectScoped := r.Group("/v1/projects",
		p.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, p.ctx),
		p.projectMiddleware(),
	)
	{
		projectScoped.GET("/:project_id", p.getProjectHandler)
		projectScoped.PUT("/:project_id", p.updateProjectHandler)
		projectScoped.DELETE("/:project_id", p.disbandProjectHandler)

		projectScoped.GET("/:project_id/members", p.listMembersHandler)
		projectScoped.POST("/:project_id/members/add", p.addMembersHandler)
		projectScoped.POST("/:project_id/members/remove", p.removeMembersHandler)
		projectScoped.POST("/:project_id/leave", p.leaveProjectHandler)
		projectScoped.PUT("/:project_id/members/:uid/role", p.updateMemberRoleHandler)
	}
}

// requireWriteEnabled is the fail-closed feature gate.
//
// Every write path goes through it; reads deliberately do not, so turning the flag
// off freezes the feature while leaving existing data observable — which is what
// makes it a usable rollback rather than a blackout.
func (p *Project) requireWriteEnabled(c *wkhttp.Context, entry string) bool {
	if p.writeEnabled() {
		return true
	}
	observeRejected(entry, reasonFlagOff)
	httperr.ResponseErrorL(c, errcode.ErrProjectDisabled, nil, nil)
	return false
}

// writeEnabled resolves the feature switch, DB → env → false.
//
// P0 read this once at construction from OCTO_PROJECT_CREATE_ENABLED, which
// meant flipping it in EITHER direction needed a rolling restart, and the value
// was not visible to clients at all — the frontend had no way to know whether to
// show the Project entry. Both are fixed by resolving through
// SystemSettings.ProjectEnabled(), which is the SAME value
// GET /v1/common/appconfig ships as project_on.
//
// One switch, two consumers. Two switches could disagree, and the disagreement
// has a worst case: the client shows the entry and every write behind it 403s.
//
// The env var still decides when no system_setting row exists, so an existing
// deployment's behaviour is byte-identical until someone writes the row.
//
// The system_setting row is an OVERRIDE, not a replacement: with no row,
// cfg.CreateEnabled — the env value resolved at construction, exactly as P0 did
// it — decides. That keeps the resolution order DB → env → false while leaving
// the env half where it was, and it is why an existing deployment behaves
// identically until someone actually writes the row.
func (p *Project) writeEnabled() bool {
	if p.settings != nil {
		if v, ok := p.settings.ProjectEnabledOverride(); ok {
			return v
		}
	}
	return p.cfg.CreateEnabled
}

// ---------- create ----------

func (p *Project) createProjectHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryProjectCreate) {
		return
	}
	spaceID := spacepkg.GetSpaceID(c)
	uid := c.GetLoginUID()

	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > maxNameChars {
		respondProjectNameInvalid(c)
		return
	}
	if utf8.RuneCountInString(req.Description) > maxDescriptionChars {
		respondProjectFieldTooLong(c, "description", maxDescriptionChars)
		return
	}
	if utf8.RuneCountInString(req.Logo) > maxLogoChars {
		respondProjectFieldTooLong(c, "logo", maxLogoChars)
		return
	}

	in := createInput{
		SpaceID:         spaceID,
		Creator:         uid,
		Name:            name,
		Description:     req.Description,
		Logo:            req.Logo,
		Discoverability: DiscoverabilitySpaceListed,
	}
	if req.Discoverability != nil {
		if !IsValidDiscoverability(*req.Discoverability) {
			respondProjectRequestInvalid(c, "discoverability")
			return
		}
		in.Discoverability = *req.Discoverability
	}
	if req.MaxMembers != nil {
		if *req.MaxMembers < 0 || *req.MaxMembers > p.cfg.MaxMembers {
			respondProjectRequestInvalid(c, "max_members")
			return
		}
		in.MaxMembers = *req.MaxMembers
	}

	model, err := p.createProject(in)
	if err != nil {
		p.respondCreateError(c, err, spaceID, uid)
		return
	}
	p.audit(auditCreate, uid, "", model.ProjectID, spaceID, "")
	// The Space role is passed as MemberRoleCommon rather than read from the database:
	// projectMiddleware has not run on this group, and the creator is the owner of the
	// project they just created, so every capability is already determined. The Space role
	// only ever widens READ visibility, which does not apply to a response about a project
	// the caller owns.
	c.Response(p.toResp(model, RoleOwner, spacepkg.MemberRoleCommon, 1))
}

// respondCreateError maps the create sentinels onto registered codes. Kept separate
// so the handler reads as validation-then-call and the mapping table lives once.
func (p *Project) respondCreateError(c *wkhttp.Context, err error, spaceID, uid string) {
	switch {
	case errors.Is(err, errQuotaPerSpace):
		observeRejected(entryProjectCreate, reasonQuotaPerSpace)
		respondProjectQuota(c, errcode.ErrProjectQuotaPerSpace, p.cfg.MaxPerSpace)
	case errors.Is(err, errQuotaPerCreator):
		observeRejected(entryProjectCreate, reasonQuotaPerCreator)
		respondProjectQuota(c, errcode.ErrProjectQuotaPerCreator, p.cfg.MaxPerCreator)
	case errors.Is(err, errQuotaDailyCreate):
		observeRejected(entryProjectCreate, reasonQuotaDailyCreate)
		respondProjectQuota(c, errcode.ErrProjectQuotaDailyCreate, p.cfg.MaxDailyCreate)
	case errors.Is(err, errNotSpaceMember):
		// The creator's Space seat was gone by the time the write transaction ran (or the
		// Space itself went inactive). entryCreateOwner exists for exactly this rejection —
		// before this fix the constant was declared and never emitted, so a create that
		// violated I1 produced no metric at all.
		observeRejected(entryCreateOwner, reasonNotSpaceMember)
		// Actor-level: this is the CREATOR's own seat, so it takes the actor-level code.
		// It used to answer with the target-level one, whose message blames "the target
		// user" — there is no target on this endpoint.
		httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
	case errors.Is(err, errNameDuplicated):
		observeRejected(entryProjectCreate, reasonNameDuplicated)
		httperr.ResponseErrorL(c, errcode.ErrProjectNameDuplicated, nil, nil)
	default:
		p.Error("创建项目失败", zap.Error(err),
			zap.String("spaceId", spaceID), zap.String("uid", uid))
		respondStoreFailed(c)
	}
}

// ---------- read ----------

func (p *Project) listProjectsHandler(c *wkhttp.Context) {
	spaceID := spacepkg.GetSpaceID(c)
	uid := c.GetLoginUID()
	offset, limit := pageParams(c)

	// `ok` is NOT discardable, and MemberRole's own doc comment says so: its role return is
	// an int whose zero value is a VALID role, so a caller that ignores ok hands ordinary
	// member rights to a non-member.
	//
	// This route's Space gate is spaceIDParamMiddleware, which answers from the shared
	// space:member:{spaceID}:{uid} cache — and that cache can hold a stale POSITIVE two ways:
	// the Space module's DEL and its negative-cache fallback both failing (a branch
	// modules/space/member_removal.go logs explicitly), or cache-aside `Set` landing after a
	// concurrent Space-side DEL and reinstating a positive entry for the full TTL. In either
	// case this read is the authoritative answer and the only one left, so it decides. It is
	// a live database read on every request, which is what makes the refusal immediate rather
	// than eventual.
	//
	// The refusal shape is the middleware's own (respondForbidden), so a caller sees the same
	// answer whether the cache was warm, cold, or stale — the alternative would make the
	// cache's state observable from the wire.
	spaceRole, isSpaceMember, err := spacepkg.MemberRole(p.ctx.DB(), spaceID, uid)
	if err != nil {
		p.Error("查询 Space 角色失败", zap.Error(err),
			zap.String("spaceId", spaceID), zap.String("uid", uid))
		respondQueryFailed(c)
		return
	}
	if !isSpaceMember {
		p.Warn("Space 成员缓存给出了过期的正命中，列表端点按库内实况拒绝",
			zap.String("spaceId", spaceID), zap.String("uid", uid))
		respondForbidden(c)
		return
	}

	rows, err := p.db.listVisibleInSpace(spaceID, uid, offset, limit)
	if err != nil {
		p.Error("查询项目列表失败", zap.Error(err), zap.String("spaceId", spaceID))
		respondQueryFailed(c)
		return
	}
	resps := make([]*Resp, 0, len(rows))
	for _, row := range rows {
		model := row.Model
		resps = append(resps, p.toResp(&model, row.MyRole, spaceRole, row.MemberCount))
	}
	c.Response(resps)
}

func (p *Project) getProjectHandler(c *wkhttp.Context) {
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	count, err := p.db.countActiveMembers(row.ProjectID)
	if err != nil {
		p.Error("统计项目成员数失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondQueryFailed(c)
		return
	}
	c.Response(p.toResp(row, requestProjectRole(c), requestSpaceRole(c), count))
}

func (p *Project) listMembersHandler(c *wkhttp.Context) {
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	// The roster is members-only (plus Space admins). A space_listed project shows
	// its metadata to any Space member, but who is in it is not part of that.
	if !canViewMembers(requestProjectRole(c), requestSpaceRole(c)) {
		httperr.ResponseErrorL(c, errcode.ErrProjectNotMember, nil, nil)
		return
	}
	offset, limit := pageParams(c)
	rows, err := p.db.listMembers(row.ProjectID, offset, limit)
	if err != nil {
		p.Error("查询项目成员失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondQueryFailed(c)
		return
	}
	resps := make([]*MemberResp, 0, len(rows))
	for _, m := range rows {
		resps = append(resps, &MemberResp{
			UID:       m.UID,
			Name:      m.Name,
			Role:      m.Role,
			InviteUID: m.InviteUID,
			CreatedAt: formatTime(m.CreatedAt),
		})
	}
	c.Response(resps)
}

// ---------- update / disband ----------

func (p *Project) updateProjectHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryProjectUpdate) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if !canUpdateProject(requestProjectRole(c)) {
		observeRejected(entryProjectUpdate, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	}

	var req updateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "")
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || utf8.RuneCountInString(name) > maxNameChars {
			respondProjectNameInvalid(c)
			return
		}
		req.Name = &name
	}
	if req.Description != nil && utf8.RuneCountInString(*req.Description) > maxDescriptionChars {
		respondProjectFieldTooLong(c, "description", maxDescriptionChars)
		return
	}
	if req.Logo != nil && utf8.RuneCountInString(*req.Logo) > maxLogoChars {
		respondProjectFieldTooLong(c, "logo", maxLogoChars)
		return
	}
	if req.Discoverability != nil && !IsValidDiscoverability(*req.Discoverability) {
		respondProjectRequestInvalid(c, "discoverability")
		return
	}
	if req.MaxMembers != nil && (*req.MaxMembers < 0 || *req.MaxMembers > p.cfg.MaxMembers) {
		respondProjectRequestInvalid(c, "max_members")
		return
	}

	uid := c.GetLoginUID()
	updated, err := p.updateFn(row.ProjectID, uid, row.SpaceID, req)
	switch {
	case err == nil:
	case errors.Is(err, errProjectGone):
		respondProjectNotFound(c)
		return
	case errors.Is(err, errPermissionDenied):
		// The in-lock re-read disagreed with the cached pre-check: the actor lost their edit
		// rights between the middleware and this transaction.
		observeRejected(entryProjectUpdate, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	case errors.Is(err, errActorNotSpaceMember):
		// The caller's own Space seat closed between the middleware's live read and this
		// transaction. An authorization refusal, so NOT store_failed: that renders Internal /
		// HTTP 500, inflates the 5xx budget, and files the rejection under the wrong reason in
		// write_rejected_total.
		observeRejected(entryProjectUpdate, reasonNotSpaceMember)
		httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
		return
	case errors.Is(err, errNoFieldsToUpdate):
		respondProjectRequestInvalid(c, "name|description|logo|discoverability|max_members")
		return
	case errors.Is(err, errNameDuplicated):
		observeRejected(entryProjectUpdate, reasonNameDuplicated)
		httperr.ResponseErrorL(c, errcode.ErrProjectNameDuplicated, nil, nil)
		return
	default:
		p.Error("更新项目失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondStoreFailed(c)
		return
	}

	p.audit(auditUpdate, uid, "", row.ProjectID, row.SpaceID, "")
	count, err := p.db.countActiveMembers(row.ProjectID)
	if err != nil {
		p.Error("统计项目成员数失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondQueryFailed(c)
		return
	}
	c.Response(p.toResp(updated, requestProjectRole(c), requestSpaceRole(c), count))
}

func (p *Project) disbandProjectHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryProjectDisband) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if !canDisbandProject(requestProjectRole(c)) {
		observeRejected(entryProjectDisband, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	}
	uid := c.GetLoginUID()
	removed, err := p.disbandFn(row.ProjectID, uid, row.SpaceID)
	switch {
	case err == nil:
	case errors.Is(err, errProjectGone):
		respondProjectNotFound(c)
		return
	case errors.Is(err, errActorNotSpaceMember):
		// See updateProjectHandler: an authorization refusal must not render as Internal 500.
		observeRejected(entryProjectDisband, reasonNotSpaceMember)
		httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
		return
	case errors.Is(err, errPermissionDenied):
		observeRejected(entryProjectDisband, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	default:
		p.Error("解散项目失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondStoreFailed(c)
		return
	}
	p.audit(auditDisband, uid, "", row.ProjectID, row.SpaceID, "",
		zap.Int("seats_closed", len(removed)))
	c.ResponseOK()
}

// ---------- response shaping ----------

func (p *Project) toResp(m *Model, myRole, spaceRole, memberCount int) *Resp {
	return &Resp{
		ProjectID:       m.ProjectID,
		SpaceID:         m.SpaceID,
		Name:            m.Name,
		Description:     m.Description,
		Logo:            m.Logo,
		Creator:         m.Creator,
		Discoverability: m.Discoverability,
		MaxMembers:      p.cfg.effectiveMaxMembers(m.MaxMembers),
		MemberCount:     memberCount,
		MemberEpoch:     m.MemberEpoch,
		Status:          m.Status,
		MyRole:          myRole,
		Capabilities:    capabilitiesFor(myRole, spaceRole),
		CreatedAt:       formatTime(m.CreatedAt),
		UpdatedAt:       formatTime(m.UpdatedAt),
	}
}

// pageParams parses offset/limit with bounds. An unbounded limit on a roster or a project
// list is an easy way to turn one authenticated request into a full-table read, so the cap
// is applied here rather than trusted from the client.
//
// maxPage is not decoration. `?page=9223372036854775807` used to overflow (page-1)*limit to
// a NEGATIVE offset, which MySQL rejects with error 1064 — so the handler answered
// err.server.project.query_failed (Internal, http_status 500). Any Space member could turn
// a query parameter into a 5xx and a stream of internal-error logs, i.e. self-serve alert
// noise. Verified by TestPageParamsCannotOverflowIntoAServerError.
//
// Clamping rather than rejecting: a page far past the end is not an error, it is an empty
// page, and that is what every other list endpoint here returns.
func pageParams(c *wkhttp.Context) (int, int) {
	const (
		defaultLimit = 50
		maxLimit     = 200
		// maxPage * maxLimit stays far inside int64 and inside any plausible table size.
		maxPage = 100000
	)
	limit := defaultLimit
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	page := 1
	if v := strings.TrimSpace(c.Query("page")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if page > maxPage {
		page = maxPage
	}
	return (page - 1) * limit, limit
}
