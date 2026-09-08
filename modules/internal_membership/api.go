package internal_membership

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

// Module is the module handle registered with the wkhttp router.
type Module struct {
	ctx           *config.Context
	store         membershipStore
	internalToken string
	log.Log
}

// New loads the token at construction. When it is unset, too short or collides
// with a sibling capability's token, the reason is logged and every request is
// refused — the module still mounts, so a misconfigured deployment fails closed
// on the endpoint rather than failing to boot the whole server.
func New(ctx *config.Context) *Module {
	logger := log.NewTLog("InternalMembership")
	token, tokenErr := resolveMembershipInternalToken(os.Getenv)
	if tokenErr != nil {
		logger.Error(tokenErr.Error())
	}
	return &Module{ctx: ctx, store: dbStore{ctx: ctx}, internalToken: token, Log: logger}
}

// Route mounts the membership endpoints under /v1/internal.
//
// Middleware EXECUTION ORDER matters and is asserted by a test:
//
//  1. per-endpoint strict IP rate limit — MUST run first, so abusive traffic
//     never reaches the token compare (a Redis Lua round trip) or the database.
//  2. internalAuthMiddleware — constant-time X-Internal-Token compare,
//     fail-closed when unset.
//
// Both are mounted on the CONCRETE routes rather than on the group. Gin runs
// group handlers ahead of route handlers, so auth-on-group + limiter-on-route
// would execute as auth → limiter: a missing or wrong token would then abort
// before consuming the strict bucket, and token-probing traffic would fall back
// to the much wider global per-IP bucket. modules/internal_resolve documents
// having been bitten by exactly this ordering.
//
// Neither route takes AuthMiddleware. That is the deliberate exclusion the
// space-isolation rule asks to be documented: these endpoints have no end user,
// the caller is a peer service authenticated by a deployment-issued token, and
// there is no uid for a user-scoped middleware to read. Space isolation is
// enforced instead by making space_id a REQUIRED part of every query predicate
// — see the handlers and pkg/project.
func (m *Module) Route(r *wkhttp.WKHttp) {
	ipLimit := m.ipRateLimit(r)
	internal := r.Group("/v1/internal")
	internal.GET("/membership/epochs", ipLimit, m.internalAuthMiddleware(), m.membershipEpochs)
	internal.POST("/project-memberships/_verify", ipLimit, m.internalAuthMiddleware(), m.verifyProjectMemberships)
}

// ipRateLimit builds the per-IP strict limiter shared by both endpoints.
//
// Shared bucket, not one per endpoint: they are called by the same peer on the
// same authorization path, and splitting the quota would only make each half
// easier to exhaust. Env values are sanitized against the defaults so a NaN /
// +Inf / non-positive typo cannot silently disable the control — ParseRPSFromEnv
// lets both NaN and +Inf through, and they surface as Lua script errors that the
// limiter treats as fail-open.
func (m *Module) ipRateLimit(r *wkhttp.WKHttp) wkhttp.HandlerFunc {
	rlRedis := octoredis.NewInstrumentedClient(m.ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	})
	rps := ratelimit.SanitizeRPS(
		wkhttp.ParseRPSFromEnv(envMembershipIPRPS, defMembershipIPRPS),
		defMembershipIPRPS,
	)
	burst := ratelimit.SanitizeBurst(
		wkhttp.ParseBurstFromEnv(envMembershipIPBurst, defMembershipIPBurst),
		defMembershipIPBurst,
	)
	return r.StrictIPRateLimitMiddleware(
		context.Background(), rlRedis, membershipRateLimitTag, rps, burst,
	)
}

// internalAuthMiddleware fails closed when the token is unset and uses a
// constant-time compare so a wrong token cannot be discovered by timing.
func (m *Module) internalAuthMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		token := c.GetHeader(internalTokenHeader)
		if m.internalToken == "" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(m.internalToken)) != 1 {
			respondUnauthorized(c)
			c.Abort()
			return
		}
		c.Next()
	}
}

// ---------- GET /v1/internal/membership/epochs ----------

// epochsResponse answers every project id the caller named.
//
// A map keyed by project id, with an entry for EVERY requested id — an unknown
// project is 0, never an absent key. The distinction matters: a consumer reading
// a missing key cannot tell "this project has no membership changes" from "my
// id never made it into the response", and the two demand opposite reactions.
//
// Zero is the sentinel the integration contract assigns to "does not exist or
// not visible", and it is fail-closed ONLY BECAUSE an active project is kept off
// it: projects are created at member_epoch 1 and the column is never written by
// anything but an increment. That invariant is what this answer rests on, not a
// coincidence about the numbers — while creation left the epoch at the column
// default of 0, a fresh solo project reported the same value as a disbanded one,
// and a consumer caching a grant under epoch 0 kept it forever. See
// modules/project migration 20260908000001.
//
// "Kept off it" rather than "cannot reach it", deliberately. Three writers keep
// the invariant, and only the first two are synchronous:
//
//   - the create path bumps the epoch, so new projects start at 1;
//   - the migration lifted every row that was already at 0;
//   - modules/project's reconcile scan repairs any row that lands back on 0
//     afterwards — a not-yet-upgraded pod mid-rollout, or a rolled-back binary,
//     both of which restore the zero-inserting create path.
//
// So the window is bounded by the scan interval, not closed outright, and the
// direction of the residue is the availability one (a brand new project reads as
// "does not exist" until the scan passes) rather than the stale-grant one. See
// modules/project.repairAbsentSentinelEpoch.
type epochsResponse struct {
	Projects map[string]int64 `json:"projects"`
}

// membershipEpochs answers "has membership changed" without answering "who is a
// member".
//
// Contract:
//   - 200 {projects: {id: epoch}} — one entry per requested id; 0 when the
//     project does not exist, is disbanded, or belongs to another Space.
//   - 400 err.shared.param.invalid — missing space_id, no ids, or too many.
//   - 401 err.shared.auth.token_invalid — token failure (middleware).
//   - 500 err.shared.internal — lookup failure. Never an empty 200: the
//     consumer must fail closed, and an empty map would read as "all gone".
//
// The endpoint deliberately cannot answer whether a given user is a member.
// Epoch is a change detector; conflating it with a membership oracle would let
// a consumer cache an authorization decision against a value that says nothing
// about that user.
func (m *Module) membershipEpochs(c *wkhttp.Context) {
	spaceID := strings.TrimSpace(c.Query("space_id"))
	if spaceID == "" {
		respondInvalidParam(c, "space_id")
		return
	}
	projectIDs, overLimit := parseIDList(c.QueryArray("project_ids"), maxBatchProjectIDs)
	if len(projectIDs) == 0 || overLimit {
		respondInvalidParam(c, "project_ids")
		return
	}

	found, err := m.store.Epochs(spaceID, projectIDs)
	if err != nil {
		m.Error("membership epochs: lookup failed",
			zap.Error(err), zap.String("space_id", spaceID), zap.Int("count", len(projectIDs)))
		respondInternal(c)
		return
	}

	out := make(map[string]int64, len(projectIDs))
	for _, id := range projectIDs {
		out[id] = found[id] // absent -> 0, which is the contract's sentinel
	}
	c.Response(epochsResponse{Projects: out})
}

// parseIDList flattens repeated query parameters and comma-separated values into
// one deduplicated, trimmed list, preserving first-seen order.
//
// Accepting both shapes costs nothing and removes a class of integration bug
// where one side sends ?ids=a&ids=b and the other expects ?ids=a,b. Duplicates
// are dropped rather than rejected here because the response is a MAP: a
// repeated id cannot produce a duplicate or missing answer the way it can in the
// verify endpoint's array, so there is nothing for strictness to protect.
//
// It stops at `limit` unique ids and reports whether more were present, so the
// caller can refuse an oversized batch WITHOUT this function first materializing
// it. A GET has no request body, so the byte cap that bounds the verify endpoint
// does not apply here; only the request-line limit does, which still admits
// thousands of ids. Parsing them all just to answer 400 would be a free
// amplification on an endpoint whose rate limit is deliberately generous.
func parseIDList(raw []string, limit int) (ids []string, overLimit bool) {
	out := make([]string, 0, limit)
	seen := make(map[string]bool, limit)
	for _, group := range raw {
		for _, part := range strings.Split(group, ",") {
			v := strings.TrimSpace(part)
			if v == "" || seen[v] {
				continue
			}
			if len(out) >= limit {
				// One past the limit is all the caller needs to know.
				return out, true
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out, false
}

// ---------- POST /v1/internal/project-memberships/_verify ----------

// verifyRequest names one project and the uids to answer for.
type verifyRequest struct {
	SpaceID   string   `json:"space_id"`
	ProjectID string   `json:"project_id"`
	UIDs      []string `json:"uids"`
}

// verifyMemberAnswer is one uid's answer.
//
// A non-member carries ONLY uid and member:false — Role is a pointer with
// omitempty so it is absent from the wire, not present as 0. Role 0 is a real
// role (ordinary member), so emitting it for a non-member would be a silent
// privilege statement, and a consumer reading `role` without first checking
// `member` would grant access. The pointer is what makes that mistake
// impossible to make by accident.
type verifyMemberAnswer struct {
	UID    string `json:"uid"`
	Member bool   `json:"member"`
	Role   *int   `json:"role,omitempty"`
}

// verifyResponse carries the project's current epoch alongside the answers.
//
// The epoch is read BEFORE the seats (see pkg/project.ProjectMemberships), so it
// is never newer than the membership data it accompanies. A consumer caching
// these answers keyed by (uid, project, member_epoch) is therefore safe: the
// worst case is an extra re-verification, never a stale grant.
type verifyResponse struct {
	ProjectID   string               `json:"project_id"`
	MemberEpoch int64                `json:"member_epoch"`
	Members     []verifyMemberAnswer `json:"members"`
}

// verifyProjectMemberships answers, for one project, whether each named uid
// still holds a seat.
//
// This is the endpoint POST /v1/auth/verify?include=context cannot replace: that
// one speaks only for the holder of the token it was given, so it can say
// nothing about an assignee, an Expert Team member or a Runtime owner.
//
// Contract:
//   - 200 {project_id, member_epoch, members[]} — exactly one answer per
//     requested uid, in request order.
//   - 400 err.shared.param.invalid — missing ids, empty/duplicate/too many uids.
//   - 401 err.shared.auth.token_invalid — token failure (middleware).
//   - 500 err.shared.internal — lookup failure.
//
// An unknown project, a disbanded one and one in another Space all answer
// member_epoch 0 with every uid member:false, rather than 404. Rejecting would
// turn the endpoint into a probe for which Space a project lives in — the same
// property modules/user's answerProjectMembership protects.
//
// It is also fail-closed, and for the reason given on epochsResponse: 0 is
// unreachable for a real project because projects start at 1, so it can never
// equal a snapshot a consumer actually took.
//
// Duplicate uids are REJECTED rather than deduplicated. The consumer's stated
// check is that every requested uid gets exactly one answer; silently collapsing
// duplicates would return fewer answers than uids and trip that check as if the
// server had dropped one. Rejecting names the caller's bug at the caller.
func (m *Module) verifyProjectMemberships(c *wkhttp.Context) {
	req, err := decodeVerifyRequest(c)
	if err != nil {
		respondInvalidParam(c, "body")
		return
	}
	spaceID := strings.TrimSpace(req.SpaceID)
	if spaceID == "" {
		respondInvalidParam(c, "space_id")
		return
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		respondInvalidParam(c, "project_id")
		return
	}
	if len(req.UIDs) == 0 || len(req.UIDs) > maxBatchUIDs {
		respondInvalidParam(c, "uids")
		return
	}
	uids := make([]string, 0, len(req.UIDs))
	seen := make(map[string]bool, len(req.UIDs))
	for _, raw := range req.UIDs {
		uid := strings.TrimSpace(raw)
		if uid == "" || seen[uid] {
			respondInvalidParam(c, "uids")
			return
		}
		seen[uid] = true
		uids = append(uids, uid)
	}

	epoch, roles, err := m.store.Memberships(spaceID, projectID, uids)
	if err != nil {
		m.Error("verify project memberships: lookup failed",
			zap.Error(err), zap.String("space_id", spaceID),
			zap.String("project_id", projectID), zap.Int("count", len(uids)))
		respondInternal(c)
		return
	}

	answers := make([]verifyMemberAnswer, 0, len(uids))
	for _, uid := range uids {
		role, ok := roles[uid]
		if !ok {
			answers = append(answers, verifyMemberAnswer{UID: uid, Member: false})
			continue
		}
		r := role
		answers = append(answers, verifyMemberAnswer{UID: uid, Member: true, Role: &r})
	}
	c.Response(verifyResponse{ProjectID: projectID, MemberEpoch: epoch, Members: answers})
}

// decodeVerifyRequest reads and strictly parses the JSON body: bounded size,
// DisallowUnknownFields so a misspelled field cannot silently be ignored, and
// rejection of trailing garbage. Mirrors modules/internal_resolve and
// modules/bot_mention.
//
// DisallowUnknownFields is load-bearing on an authorization endpoint: a caller
// that sends `uid` instead of `uids` should get a 400, not a successful 200 with
// an empty answer set that reads as "nobody is a member".
func decodeVerifyRequest(c *wkhttp.Context) (verifyRequest, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var req verifyRequest
	if err := decoder.Decode(&req); err != nil {
		return verifyRequest{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return verifyRequest{}, errors.New("verify request contains multiple JSON values")
		}
		return verifyRequest{}, err
	}
	return req, nil
}
