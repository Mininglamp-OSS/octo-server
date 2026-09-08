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

	// nudgeProvisioningFn is the post-create worker trigger, as an instance seam on the
	// same terms as the five above: production gets the real goroutine, and a test that
	// needs to observe the outbox BEFORE the worker touches it swaps this on its own
	// instance. Without a seam the nudge races every assertion about a pending row —
	// and the alternative (sleeping until it settles) would make the outbox's own
	// timing the thing under test.
	nudgeProvisioningFn func()

	// provisionClient is the ONLY outbound path to fleet / drive, and it is reachable
	// only from provisioning_worker.go — no handler in this module may touch it (see
	// that file's header and TestProvisioningClientIsConfinedToTheWorker). Non-nil even
	// when provisioning is disabled, because the worker that would use it does not start
	// in that case and a nil check per job would be a second place to get wrong.
	provisionClient provisionEnsurer

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

	p.provisionClient = newProvisionClient()
	p.nudgeProvisioningFn = p.nudgeProvisioningWorker

	p.registerSpaceMemberRemovalCleanup()
	p.registerAllMemberGroupOwnerFinalizer()
	// Publish the provisioning configuration verdict at CONSTRUCTION, not in Route():
	// a rejected target must be visible even in a crash loop that never reaches Route,
	// and a startup log line alone is lost within minutes.
	p.publishProvisioningConfigMetrics()
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
	// 项目侧成员移除级联 worker（D5）。与 Space 那条清理工单各自独立：键不同、
	// 扇出规模不同，且 Space 的步骤契约规定「任一步骤报错整单重跑」——挂在一起会
	// 让项目侧的失败去重跑 Space 侧已成功的步骤。
	p.startRemovalWorker()
	// Inert unless a target is enabled; see startProvisioningWorker.
	p.startProvisioningWorker()
	// The census runs regardless, so a rollback that clears the target list does not take
	// the gauges with it — see startProvisioningMetrics.
	p.startProvisioningMetrics()

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

		// Read-only, and gated by the caller's own group membership rather than by
		// a role check — see listProjectGroupsHandler.
		projectScoped.GET("/:project_id/groups", p.listProjectGroupsHandler)

		// Personal preferences for one project (pinning). A settings bag rather
		// than /pin + /unpin, mirroring PUT /v1/groups/:group_no/setting.
		projectScoped.PUT("/:project_id/setting", p.updateSettingHandler)

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
	// agent_uids is bounded here and re-decided in the transaction.
	//
	// Only the SHAPE is checked at this layer: the count. Eligibility needs the
	// Space seats locked, so it belongs inside the create transaction — a check
	// out here would be a read that can expire between the check and the write,
	// which is the same reason P0 moved the creator's own I1 check inside.
	//
	// The cap reuses MemberBatchMax rather than inventing a second one: these are
	// membership writes in a batch, exactly like members/add, and two caps for the
	// same shape of request drift apart.
	in.AgentUIDs = sanitizeUIDs(req.AgentUIDs)
	if len(in.AgentUIDs) > p.cfg.MemberBatchMax {
		respondProjectBatchTooLarge(c, p.cfg.MemberBatchMax)
		return
	}

	model, err := p.createProject(in)
	if err != nil {
		p.respondCreateError(c, err, spaceID, uid, in.MaxMembers)
		return
	}
	p.audit(auditCreate, uid, "", model.ProjectID, spaceID, "")
	// D2/D3 — the agent seats written inside the create transaction are member adds
	// like any other, and the acceptance criterion is that EVERY write path leaves an
	// audit entry. Without these, the only membership write on this endpoint that the
	// trail records is the owner seat, and an agent that gained access to the project
	// at creation is invisible to whoever later asks how it got there.
	//
	// Emitted here, after createProject returned nil, and only then: the create is
	// whole-request (D3), so success means every uid in the list was seated. Emitting
	// inside the transaction would log seats a rollback then discarded.
	//
	// Filtered through withoutUID for the same reason createProjectOnce filters the
	// list before seating anything: a caller who names THEMSELVES in agent_uids has
	// that uid dropped, so auditing the raw request list would record a membership
	// write that never happened.
	for _, agentUID := range withoutUID(in.AgentUIDs, uid) {
		p.audit(auditMemberAdd, uid, agentUID, model.ProjectID, spaceID, auditReasonAgentOnCreate)
	}
	// The Space role is passed as MemberRoleCommon rather than read from the database:
	// projectMiddleware has not run on this group, and the creator is the owner of the
	// project they just created, so every capability is already determined. The Space role
	// only ever widens READ visibility, which does not apply to a response about a project
	// the caller owns.
	humans, agents := p.splitSeatCounts(model.ProjectID)
	// pinned is false and that is provable, not assumed: this project id was
	// generated inside the transaction that just committed, so no settings row
	// for it can exist yet.
	c.Response(p.toResp(model, RoleOwner, spacepkg.MemberRoleCommon, humans, agents, false))
}

// respondCreateError maps the create sentinels onto registered codes. Kept separate
// so the handler reads as validation-then-call and the mapping table lives once.
//
// It deliberately does NOT take the request's agent_uids: the only arm that needs
// uids needs the ineligible SUBSET, which travels on the error itself. Handing the
// full submitted list in as a parameter is what made echoing all of them the easy
// thing to write.
func (p *Project) respondCreateError(c *wkhttp.Context, err error, spaceID, uid string, maxMembers int) {
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
	case errors.Is(err, errProvisioningEnqueueFailed):
		// Same wire answer as any other store failure — a client cannot act on it —
		// but its own metric reason, so an operator can tell that create started
		// failing because the provisioning outbox write failed rather than because the
		// project write did. This slice is what made create depend on that write.
		observeRejected(entryProjectCreate, reasonProvisioningEnqueue)
		p.Error("创建项目失败：子系统预置工单写入失败", zap.Error(err),
			zap.String("spaceId", spaceID), zap.String("uid", uid))
		respondStoreFailed(c)
	case errors.Is(err, errAgentNotEligible):
		// D3 — one ineligible agent rejects the whole create, and all seven
		// reasons render identically. The uids echoed in details are the INELIGIBLE
		// SUBSET, carried out of the transaction by agentNotEligibleError, so a
		// picker highlights the rows the user actually has to fix rather than every
		// row they picked; the reasons went to the log inside createProjectOnce.
		//
		// A bare sentinel with no subset attached echoes nothing: guessing would
		// mean naming uids that may well have been fine.
		observeRejected(entryProjectCreate, reasonAgentNotEligible)
		var ineligible *agentNotEligibleError
		if errors.As(err, &ineligible) {
			respondProjectAgentNotEligible(c, ineligible.UIDs)
		} else {
			respondProjectAgentNotEligible(c, nil)
		}
	case errors.Is(err, errQuotaMembers):
		// Reachable from create only via agent_uids: the owner seat alone cannot
		// exceed a member quota. Before agents existed this arm did not need to be
		// here, and without it the refusal would fall through to store_failed —
		// an Internal 5xx for what is a caller error with a registered code.
		observeRejected(entryProjectCreate, reasonQuotaMembers)
		// effectiveMaxMembers, not the global cap: the request may carry its own
		// max_members, and that is the number the refusal was measured against.
		// Reporting the global one tells the caller a limit their project does not
		// have, which a client renders straight into a hint.
		respondProjectQuota(c, errcode.ErrProjectQuotaMembers, p.cfg.effectiveMaxMembers(maxMembers))
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
		// The split comes from the list query itself (two bounded correlated
		// subqueries), so a list card and the detail route agree about what
		// member_count means. Reporting the full seat count here while the detail
		// route reported humans only would have been D16's own bug, one endpoint
		// away.
		resps = append(resps, p.toResp(&model, row.MyRole, spaceRole, row.MemberCount, row.AgentCount(), row.Pinned == 1))
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
	humans, agents := p.splitSeatCounts(row.ProjectID)
	pinned, err := p.db.queryProjectPinned(row.ProjectID, c.GetLoginUID())
	if err != nil {
		p.Error("查询项目置顶状态失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondQueryFailed(c)
		return
	}
	c.Response(p.toResp(row, requestProjectRole(c), requestSpaceRole(c), humans, agents, pinned))
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
			// D16 — the roster tells people and agents apart, and names each
			// agent's owner, so a client can nest agents under their owner the
			// way the Space directory does. Both come from LEFT JOINs, so a
			// member with no user row reads as robot=0 and owner_uid="" rather
			// than dropping out of the roster.
			Robot:     m.Robot,
			OwnerUID:  m.OwnerUID,
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
	// The count no longer fails the response. It never should have: the update
	// COMMITTED, and answering 500 because a display aggregate hiccuped tells the
	// caller their write failed when it did not, which is the one thing a response
	// after a successful write must not do.
	humans, agents := p.splitSeatCounts(updated.ProjectID)
	// The pin survives a rename, so it has to be re-read rather than defaulted:
	// reporting false here would make the caller watch their own card leave the
	// pinned section until the next list fetch.
	pinned, err := p.db.queryProjectPinned(updated.ProjectID, c.GetLoginUID())
	if err != nil {
		p.Error("查询项目置顶状态失败", zap.Error(err), zap.String("projectId", updated.ProjectID))
		respondQueryFailed(c)
		return
	}
	c.Response(p.toResp(updated, requestProjectRole(c), requestSpaceRole(c), humans, agents, pinned))
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

// toResp renders a project. memberCount counts HUMANS and agentCount counts AI
// agents (D16); MaxMembers still bounds the two together, because a seat is a
// seat regardless of who sits in it.
// toResp shapes one project for the wire.
//
// pinned is a parameter rather than a field on Model because it is a fact about
// the CALLER, not about the project: the same row is pinned for one user and not
// for the next. Every call site must therefore supply it truthfully — a default of
// false would make the update route report a project as un-pinned right after the
// caller renamed it, and the client would watch its own card jump out of the
// pinned section until the next list fetch.
func (p *Project) toResp(m *Model, myRole, spaceRole, memberCount, agentCount int, pinned bool) *Resp {
	return &Resp{
		ProjectID:        m.ProjectID,
		SpaceID:          m.SpaceID,
		Name:             m.Name,
		Description:      m.Description,
		Logo:             m.Logo,
		Creator:          m.Creator,
		Discoverability:  m.Discoverability,
		MaxMembers:       p.cfg.effectiveMaxMembers(m.MaxMembers),
		MemberCount:      memberCount,
		AgentCount:       agentCount,
		MemberEpoch:      m.MemberEpoch,
		Status:           m.Status,
		AllMemberGroupNo: m.AllMemberGroupNo,
		Pinned:           pinned,
		MyRole:           myRole,
		Capabilities:     capabilitiesFor(myRole, spaceRole),
		CreatedAt:        formatTime(m.CreatedAt),
		UpdatedAt:        formatTime(m.UpdatedAt),
	}
}

// splitSeatCounts renders the human/agent split for one project, degrading to
// "everything is a human" if the query fails.
//
// Degrading rather than failing the whole response is deliberate: the split is a
// display refinement, and a project detail page that 500s because a COUNT with a
// join to `user` hiccuped is a worse answer than one whose agent badge is
// missing. The total is unaffected either way — humans+agents always equals the
// seat count the quota uses.
//
// # The fallback total is read HERE, not by the caller
//
// Every caller used to run countActiveMembers first and hand the result in, so
// the happy path paid for two aggregates and used one — the second existed
// purely to feed a branch that does not run. PR #855's review measured it as one
// full aggregate per project detail, update and create response.
//
// Reading it inside the failure branch keeps the degrade and makes the happy path
// a single query. It also fixes the create path's fallback, which passed
// `1 + len(agent_uids)` — a number computed from the REQUEST, and therefore wrong
// by one whenever the caller named themselves in agent_uids (createProjectOnce
// strips the creator before seating anything).
//
// If the fallback count fails too, report zeros: two aggregates over the same
// table both failing is not a display hiccup, and inventing a total from the
// request is what produced the off-by-one above.
func (p *Project) splitSeatCounts(projectID string) (humans, agents int) {
	humans, agents, err := p.db.countActiveSeatsByKind(projectID)
	if err == nil {
		return humans, agents
	}
	p.Warn("统计项目成员构成失败，回退到只数总席位",
		zap.Error(err), zap.String("projectId", projectID))
	total, countErr := p.db.countActiveMembers(projectID)
	if countErr != nil {
		p.Warn("回退统计项目成员数也失败，成员数按 0 下发",
			zap.Error(countErr), zap.String("projectId", projectID))
		return 0, 0
	}
	return total, 0
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
