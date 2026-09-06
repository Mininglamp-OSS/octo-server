package project

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// Cache TTLs, mirroring pkg/space/middleware.go's cacheTTL / negativeCacheTTL so a
// new member's admission becomes visible on the same schedule the Space gate uses.
const (
	projectCacheTTL         = 60 * time.Second
	projectNegativeCacheTTL = 30 * time.Second
)

// gin context keys. Written by the middleware, read by handlers.
const (
	ctxKeyProjectRow  = "project_row"
	ctxKeyProjectRole = "project_role"
	ctxKeySpaceRole   = "project_space_role"
)

// projectMemberCacheKey namespaces the project membership cache under `project:`,
// which collides with no existing prefix (checked against `space:member:*` and
// `ratelimit:*`).
//
// Note what is NOT cached here: the caller's SPACE membership. That check goes
// through spacepkg's own RedisMembershipCache and therefore through the SAME
// `space:member:{spaceID}:{uid}` key the Space module invalidates synchronously
// when it removes someone. Minting a second copy of that fact under a `project:`
// key would leave nobody to invalidate it, and a member removed from the Space
// would keep passing the Project Space-gate for up to a full TTL. The `project:*`
// namespace requirement applies to project membership; reusing `space:*` for Space
// membership is a correctness requirement, not a naming oversight.
func projectMemberCacheKey(projectID, uid string) string {
	return fmt.Sprintf("project:member:%s:%s", projectID, uid)
}

// errProjectCacheNegativeFallback marks "DEL failed, but a negative entry was
// written over the top".
//
// The two non-nil returns from invalidateProjectMemberCache mean opposite things
// operationally, and reporting them as one thing reports them WRONG:
//
//   - this sentinel: the stale positive entry survived, but a shorter-lived "not a
//     member" entry now shadows it, so the very next request is refused. The
//     boundary held; what is left is dirt that expires.
//   - not this sentinel (DEL and the fallback write both failed): the positive
//     entry lives out its full TTL and a removed member keeps their access. That
//     is a real authorization failure.
//
// The fallback-succeeded case is the more common of the two. Reporting it as
// "possibly still authorized" sends on-call chasing an escalation that did not
// happen, and after a few of those nobody reads the line any more.
var errProjectCacheNegativeFallback = errors.New("project member cache: negative fallback written")

// projectCacheStore is the minimal Redis surface the invalidation path needs.
// Extracted as an interface so the "DEL failed" branch — the only dangerous one,
// and one a real Redis will not produce on demand — is testable. Same seam as
// pkg/space's membershipCacheStore.
type projectCacheStore interface {
	Del(key string) error
	SetAndExpire(key string, value interface{}, expire time.Duration) error
}

// invalidateProjectMemberCacheIn is the injectable core of the invalidation path.
//
// Redis being entirely down is the SAFE failure: the middleware's Get misses,
// falls back to the database, and a removed member is refused immediately. The
// dangerous failure is DEL alone failing, because the positive entry then survives
// its full TTL while the handler has already committed and returned 200. So when
// DEL fails, overwrite instead of merely logging: a shorter-lived negative entry
// makes the middleware refuse on the next request.
// The fallback direction is right for REMOVALS and wrong for GRANTS, and this helper runs on
// both. On a DEL failure after an add or a promotion, the negative entry denies a member whose
// seat has just committed, for up to projectNegativeCacheTTL (30s). That is the fail-SAFE
// direction — a false denial for 30s, versus a removed member keeping authority for a full
// positive TTL — and it is kept deliberately, with the same reasoning modules/space applies to
// its own membership cache. A positive fallback for grant paths would have to write a role it
// believes rather than one it read under the lock, which is how a stale positive gets minted.
// Raised in both review rounds of PR #841; the answer is "yes, and on purpose".
func invalidateProjectMemberCacheIn(store projectCacheStore, projectID, uid string) error {
	key := projectMemberCacheKey(projectID, uid)
	delErr := store.Del(key)
	if delErr == nil {
		return nil
	}
	if setErr := store.SetAndExpire(key, strconv.Itoa(roleNonMember), projectNegativeCacheTTL); setErr != nil {
		return fmt.Errorf("invalidate project member cache: del failed (%w) and negative-cache fallback also failed (%v)", delErr, setErr)
	}
	return fmt.Errorf("invalidate project member cache: del failed (%w); %w", delErr, errProjectCacheNegativeFallback)
}

// invalidateProjectMemberCache drops one member's cached project role.
//
// Called SYNCHRONOUSLY, inside the request, after every membership write. Handing
// it to a worker would leave a removed member authorized for up to a full TTL,
// which is the same rule modules/space follows for its own membership cache and
// for the same reason: this is an authorization boundary, not a display cache.
func (p *Project) invalidateProjectMemberCache(projectID, uid string) {
	if projectID == "" || uid == "" {
		return
	}
	conn := p.ctx.GetRedisConn()
	if conn == nil {
		// Without Redis the middleware cannot read a cached entry either, so its
		// Get misses and falls back to the database. Safe direction.
		return
	}
	err := invalidateProjectMemberCacheIn(conn, projectID, uid)
	switch {
	case err == nil:
	case errors.Is(err, errProjectCacheNegativeFallback):
		p.Warn("清理项目成员缓存：DEL 失败，已写否定缓存兜底，鉴权仍然生效",
			zap.String("projectId", projectID), zap.String("uid", uid), zap.Error(err))
	default:
		p.Error("清理项目成员缓存失败：被移除成员可能在缓存 TTL 内仍可访问该项目",
			zap.String("projectId", projectID), zap.String("uid", uid), zap.Error(err))
	}
}

// cachedProjectRole reads the caller's project role, preferring Redis.
//
// The cached value is the role itself rather than a boolean, because every handler
// needs the role and a boolean cache would force a second database read on exactly
// the requests the cache was meant to spare. roleNonMember (-1) is cached too, on
// the shorter negative TTL, so a probe for a project the caller is not in does not
// hit the database every time.
func (p *Project) cachedProjectRole(projectID, uid string) (int, error) {
	conn := p.ctx.GetRedisConn()
	key := projectMemberCacheKey(projectID, uid)
	if conn != nil {
		if val, err := conn.GetString(key); err == nil && val != "" {
			if role, convErr := strconv.Atoi(val); convErr == nil {
				return role, nil
			}
			// A corrupt value must not be trusted as a role: fall through to the
			// database rather than defaulting to 0, which is RoleCommon.
		}
	}

	role := roleNonMember
	member, err := p.db.queryMember(projectID, uid)
	if err != nil {
		return roleNonMember, err
	}
	// D4 — a seat with removing = 1 is NOT a member for any authorization
	// purpose, even though its status is still active. This is the middleware
	// half of that rule; pkg/project's predicates carry the other half, and the
	// two must agree or "who is a member" depends on which door you came in.
	if member != nil && member.Status == MemberStatusActive && member.Removing == 0 {
		role = member.Role
	}

	if conn != nil {
		ttl := projectCacheTTL
		if role == roleNonMember {
			ttl = projectNegativeCacheTTL
		}
		_ = conn.SetAndExpire(key, strconv.Itoa(role), ttl)
	}
	return role, nil
}

// spaceMembershipCache is the Space-gate cache. Constructed from pkg/space so it
// shares the key format and therefore the Space module's synchronous invalidation.
func (p *Project) checkSpaceMembership(spaceID, uid string) (bool, error) {
	// Without Redis the cache cannot answer — and would panic if asked. Degrade to the
	// database, which is the safe direction (same reasoning as invalidateProjectMemberCache).
	// spaceCache is nil exactly when the process has no Redis connection at all; see New.
	if p.spaceCache == nil {
		return spacepkg.CheckMembership(p.ctx.DB(), spaceID, uid)
	}
	if isMember, found := p.spaceCache.Get(spaceID, uid); found {
		return isMember, nil
	}
	isMember, err := spacepkg.CheckMembership(p.ctx.DB(), spaceID, uid)
	if err != nil {
		return false, err
	}
	ttl := projectCacheTTL
	if !isMember {
		ttl = projectNegativeCacheTTL
	}
	p.spaceCache.Set(spaceID, uid, isMember, ttl)
	return isMember, nil
}

// spaceIDParamMiddleware resolves and verifies the Space named by the :space_id
// PATH parameter.
//
// It exists because pkg/space's SpaceMiddleware cannot be used here: that
// middleware reads only the `space_id` query parameter and the `X-Space-ID`
// header, and when neither is present it calls c.Next() — it PASSES
// (pkg/space/middleware.go:158-165). A route keyed on a path parameter would
// therefore be completely unguarded under it. The context key is the same one, via
// the exported spacepkg.SetSpaceID, so handlers reading GetSpaceID behave
// identically whichever middleware ran.
//
// It also deliberately does NOT copy SpaceMiddleware's failure exits: those use
// raw c.AbortWithStatusJSON(status, gin.H{"msg": "..."}) with hardcoded Chinese,
// which bypasses the localized envelope. Every exit here goes through httperr.
func (p *Project) spaceIDParamMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		spaceID := c.Param("space_id")
		if spaceID == "" {
			respondParamInvalid(c, "space_id")
			c.Abort()
			return
		}
		uid := c.GetLoginUID()
		if uid == "" {
			respondNotLoggedIn(c)
			c.Abort()
			return
		}
		isMember, err := p.checkSpaceMembership(spaceID, uid)
		if err != nil {
			p.Error("校验 Space 成员身份失败", zap.Error(err),
				zap.String("spaceId", spaceID), zap.String("uid", uid))
			respondQueryFailed(c)
			c.Abort()
			return
		}
		if !isMember {
			respondForbidden(c)
			c.Abort()
			return
		}
		spacepkg.SetSpaceID(c, spaceID)
		c.Next()
	}
}

// projectMiddleware resolves the :project_id path parameter into a verified
// project, its Space, and the caller's role in both.
//
// Three refusals all render the SAME anti-enumeration response
// (respondProjectNotFound), and that sameness is the security property:
//
//  1. the project does not exist;
//  2. it exists in a Space the caller is not a member of;
//  3. it exists, is unlisted, and the caller is neither a member nor a Space admin.
//
// Answering 403 for (2) would tell an outsider that a given project id is real and
// which of their probes landed in a foreign tenant. Same shape as modules/channel
// folding not-found into forbidden (modules/channel/api.go:179-194). The
// distinguishing reason goes to the log line only.
//
// A disbanded project is case (1) for everyone. Note this is NOT the
// project_create_enabled flag's behaviour — that gate keeps reads working; disband
// is terminal.
func (p *Project) projectMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		projectID := c.Param("project_id")
		if projectID == "" {
			respondParamInvalid(c, "project_id")
			c.Abort()
			return
		}
		uid := c.GetLoginUID()
		if uid == "" {
			respondNotLoggedIn(c)
			c.Abort()
			return
		}

		row, err := p.db.queryByProjectID(projectID)
		if err != nil {
			p.Error("查询项目失败", zap.Error(err), zap.String("projectId", projectID))
			respondQueryFailed(c)
			c.Abort()
			return
		}
		if row == nil || row.Status != StatusNormal {
			p.Debug("项目不存在或已解散", zap.String("projectId", projectID), zap.String("uid", uid))
			respondProjectNotFound(c)
			c.Abort()
			return
		}

		// Space membership and Space role answer the SAME predicate
		// (space_member.status=1 AND space.status=1), so they are ONE read, not two: MemberRole
		// returns ok=false exactly when CheckMembership would return false. The earlier version
		// ran the cached membership check here and then an uncached MemberRole — the cache saved
		// nothing on this route, because the role query hit the same tables anyway
		// (yujiawei Q8, PR #841 round 1).
		//
		// The role is deliberately NOT cached: it widens what the caller may see, so a demotion
		// has to take effect at once, and a second cached authorization fact with no
		// invalidation path is the bug this module avoids everywhere else.
		spaceRole, isMember, err := spacepkg.MemberRole(p.ctx.DB(), row.SpaceID, uid)
		if err != nil {
			p.Error("校验 Space 成员身份失败", zap.Error(err),
				zap.String("spaceId", row.SpaceID), zap.String("uid", uid))
			respondQueryFailed(c)
			c.Abort()
			return
		}
		if !isMember {
			p.Debug("调用者不在项目所属 Space，按不存在响应",
				zap.String("projectId", projectID), zap.String("spaceId", row.SpaceID),
				zap.String("uid", uid))
			respondProjectNotFound(c)
			c.Abort()
			return
		}
		// Only NOW is this Space verified for this caller: SetSpaceID means "verified", and
		// writing it before the check would hand any handler reading GetSpaceID a Space the
		// caller has no seat in.
		spacepkg.SetSpaceID(c, row.SpaceID)

		projectRole, err := p.cachedProjectRole(projectID, uid)
		if err != nil {
			p.Error("查询项目成员角色失败", zap.Error(err),
				zap.String("projectId", projectID), zap.String("uid", uid))
			respondQueryFailed(c)
			c.Abort()
			return
		}

		if row.Discoverability == DiscoverabilityUnlisted &&
			projectRole == roleNonMember && spaceRole < spacepkg.MemberRoleAdmin {
			p.Debug("unlisted 项目对非成员按不存在响应",
				zap.String("projectId", projectID), zap.String("uid", uid))
			respondProjectNotFound(c)
			c.Abort()
			return
		}

		c.Set(ctxKeyProjectRow, row)
		c.Set(ctxKeyProjectRole, projectRole)
		c.Set(ctxKeySpaceRole, spaceRole)
		c.Next()
	}
}

// ---------- context accessors ----------

// requestProject returns the project resolved by projectMiddleware. A nil return
// means the middleware did not run, which is a wiring bug rather than a request
// error, so callers respond 500 rather than 404.
func requestProject(c *wkhttp.Context) *Model {
	if v, ok := c.Get(ctxKeyProjectRow); ok {
		if m, ok := v.(*Model); ok {
			return m
		}
	}
	return nil
}

// requestProjectRole returns the caller's project role, or roleNonMember.
func requestProjectRole(c *wkhttp.Context) int {
	if v, ok := c.Get(ctxKeyProjectRole); ok {
		if role, ok := v.(int); ok {
			return role
		}
	}
	return roleNonMember
}

// requestSpaceRole returns the caller's Space role, or MemberRoleCommon.
func requestSpaceRole(c *wkhttp.Context) int {
	if v, ok := c.Get(ctxKeySpaceRole); ok {
		if role, ok := v.(int); ok {
			return role
		}
	}
	return spacepkg.MemberRoleCommon
}
