package internal_membership

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
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
		// Cleared explicitly rather than relying on every error path in
		// resolveMembershipInternalToken returning "". They all do today, and
		// the middleware refuses on an empty token — but that makes fail-closed
		// a property of four separate return statements instead of one line
		// here. One future path returning a partially-validated value would
		// enable the capability with an error already logged.
		token = ""
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
	deadline := boundBodyReadTime()
	internal := r.Group("/v1/internal")
	internal.GET("/membership/epochs", deadline, ipLimit, m.internalAuthMiddleware(), m.membershipEpochs)
	internal.POST("/project-memberships/_verify", deadline, ipLimit, m.internalAuthMiddleware(), m.verifyProjectMemberships)
}

// ipRateLimit builds the per-IP strict limiter shared by both endpoints.
//
// Shared bucket, not one per endpoint: they are called by the same peer on the
// same authorization path, and splitting the quota would only make each half
// easier to exhaust. Env values are sanitized against the defaults so a NaN /
// +Inf / non-positive typo cannot silently disable the control — ParseRPSFromEnv
// lets both NaN and +Inf through, and they surface as Lua script errors that the
// limiter treats as fail-open.
// Lifecycle: the client below is never closed, because Route() runs exactly once
// per process and the limiter it builds lives as long as the router. A module
// with a Stop hook (cardActionDispatchRuntime) closes its client; this one has
// none to hang the close on. If a Stop hook is ever added here, the client goes
// with it — repeated construction in a test or a hot reload would otherwise leak
// a pool per call.
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
// "Requested id" means the id after separator whitespace is trimmed, because the
// ids arrive in a comma-separated query parameter where that whitespace is
// syntax (see parseIDList). For a caller sending canonical ids — the contract's
// form — trimmed and sent are the same string. The verify endpoint takes its ids
// from a JSON body, where there is no separator to absorb, and rejects a padded
// id rather than answering under a different key.
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
// "Kept off it" rather than "cannot reach it", deliberately. Four mechanisms
// keep the invariant, and only the last two run on every request:
//
//   - the create path bumps the epoch, so new projects start at 1;
//   - the migration lifted every row that was already at 0;
//   - modules/project's reconcile scan repairs any row that lands back on 0
//     afterwards — a not-yet-upgraded pod mid-rollout, or a rolled-back binary,
//     both of which restore the zero-inserting create path;
//   - and, because the first three are all one-shots or scheduled, the READ
//     layer refuses to serve an ACTIVE project found on the sentinel at all
//     (pkg/project.ErrLiveProjectOnAbsentSentinel → 500 here).
//
// The last one is what makes the claim structural instead of a statement about
// scan latency, and an earlier version of this comment got the reason wrong. It
// said the residue during the window was "the availability direction" only. That
// is false: serving a live project at 0 lets the peer cache a positive grant
// keyed on 0, and if the project is then disbanded before the scan repairs it —
// disband bumps the epoch and then flips status, so the row leaves the epochs
// predicate and the answer becomes 0 again — the consumer's staleness check
// agrees with its own cached 0 and the grant never expires. Reproduced against
// the engine; see TestRolloutSentinelIsRefusedRatherThanServed.
//
// So the window now costs availability (500, retry, repaired within one scan
// rotation) rather than a permanent grant. It also means the rollback runbook no
// longer DEPENDS on the reconcile loop being enabled: with the loop off the
// endpoint refuses instead of quietly handing out the collision.
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
	// space_id is single-valued, so — unlike project_ids — there is no separator
	// syntax for whitespace to belong to and a padded value is malformed. Taken
	// verbatim here so both endpoints agree about the same field name; the verify
	// endpoint made this choice first and the two disagreeing was the bug.
	spaceID := c.Query("space_id")
	if spaceID == "" || spaceID != strings.TrimSpace(spaceID) {
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
		m.logLookupFailure("membership epochs", err, spaceID, len(projectIDs))
		respondInternal(c)
		return
	}

	out := make(map[string]int64, len(projectIDs))
	byFold := foldKeys(len(found))
	for id, epoch := range found {
		byFold[foldID(id)] = epoch
	}
	for _, id := range projectIDs {
		// Keyed by what the CALLER asked, matched case-insensitively — see foldID.
		// Absent -> 0, which is the contract's sentinel.
		out[id] = byFold[foldID(id)]
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
//
// The comma split is a strings.Cut loop rather than strings.Split so THIS
// function does not allocate a slice for the whole group before the limit is
// consulted: one `?project_ids=` value with a hundred thousand commas used to
// allocate a hundred thousand strings on the way to a 400.
//
// Scoped honestly, because an earlier version of this comment claimed more than
// it delivered. The query string is ALREADY materialized before this is reached
// — gin's c.QueryArray goes through url.ParseQuery — so the saving is one
// allocation pass, not the whole input; and the loop still walks every duplicate
// and empty field past the limit, since only UNIQUE ids count against it. Both
// are bounded by the server's request-line/header limits and both sit behind
// token auth, so what is left is a constant factor, not a hazard.
//
// Whitespace around a comma is SEPARATOR syntax here, so it is trimmed: a caller
// that builds the query with `strings.Join(ids, ", ")` means the ids, not the
// spaces. The consequence is that the answer is keyed by the TRIMMED id, which is
// what the caller's own id was in that shape. An id whose value genuinely
// contains surrounding whitespace is therefore not round-tripped — but such an id
// is outside the contract's canonical-uuid form, and the verify endpoint, whose
// uids arrive in a JSON array where whitespace is unambiguously part of the
// value, rejects that shape outright rather than trimming it.
func parseIDList(raw []string, limit int) (ids []string, overLimit bool) {
	out := make([]string, 0, limit)
	seen := make(map[string]bool, limit)
	for _, group := range raw {
		rest := group
		for {
			part, after, more := strings.Cut(rest, ",")
			v := strings.TrimSpace(part)
			switch {
			case v == "" || seen[v]:
			case len(out) >= limit:
				// One past the limit is all the caller needs to know.
				return out, true
			default:
				seen[v] = true
				out = append(out, v)
			}
			if !more {
				break
			}
			rest = after
		}
	}
	return out, false
}

// foldID normalizes an identifier for matching, and foldKeys builds the map it
// keys.
//
// # Why the answers cannot be keyed by what the DATABASE returned
//
// octo_project and octo_project_member are pinned to utf8mb4_general_ci, which is
// case-INSENSITIVE. So `project_id IN (?)` matches a stored `abc` when the caller
// sends `ABC`, and the row comes back spelled `abc`. Keying the response off that
// returned spelling means a caller looking up its own `ABC` finds nothing — and
// an absent key is the contract's "does not exist" / member:false sentinel. A real
// member of a live project would read as denied, and a whole project as absent,
// while the SQL had matched perfectly.
//
// That contradicts what this module promises for itself: every answer carries the
// identifier the caller sent, so a consumer keying by its own strings always finds
// it. The peer persists project_id in a different engine and reads it back; there
// is no guarantee the spelling survives that round trip unchanged, and it is not
// this endpoint's business to require that it does.
//
// So the answer is keyed by the caller's string and MATCHED by the folded form.
//
// Fold, not reject. Rejecting non-canonical case was the alternative and it is
// worse: it would break a caller holding a legitimately re-cased id, and the
// endpoint has no basis to declare one spelling canonical — existing project ids
// predate the UUID format and nothing validates their shape.
//
// ASCII-only folding, deliberately. It is not a reimplementation of
// utf8mb4_general_ci — matching that exactly would mean tracking a MySQL collation
// table in Go. Identifiers here are hex UUIDs and generated uids, where the two
// agree. Where they could disagree, this fold is the STRICTER of the two, so the
// disagreement costs an answer of "absent" rather than a wrong positive: the
// fail-closed direction.
func foldID(id string) string { return strings.ToLower(id) }

func foldKeys(n int) map[string]int64 { return make(map[string]int64, n) }

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
	// Every field of a JSON body is the value itself: unlike the query string,
	// there is no separator syntax for whitespace to belong to. So a padded value
	// is a malformed one and is refused rather than silently rewritten — which
	// also keeps the response honest, since it echoes project_id and every uid
	// back and a caller keying on their own strings must find them there.
	spaceID := req.SpaceID
	if spaceID == "" || spaceID != strings.TrimSpace(spaceID) {
		respondInvalidParam(c, "space_id")
		return
	}
	projectID := req.ProjectID
	if projectID == "" || projectID != strings.TrimSpace(projectID) {
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
		uid := raw
		if uid == "" || uid != strings.TrimSpace(uid) || seen[uid] {
			respondInvalidParam(c, "uids")
			return
		}
		seen[uid] = true
		uids = append(uids, uid)
	}

	epoch, roles, err := m.store.Memberships(spaceID, projectID, uids)
	if err != nil {
		m.logLookupFailure("verify project memberships", err, spaceID, len(uids),
			zap.String("project_id", projectID))
		respondInternal(c)
		return
	}

	byFold := foldKeys(len(roles))
	for uid, role := range roles {
		byFold[foldID(uid)] = int64(role)
	}
	answers := make([]verifyMemberAnswer, 0, len(uids))
	for _, uid := range uids {
		folded, ok := byFold[foldID(uid)]
		role := int(folded)
		if !ok {
			answers = append(answers, verifyMemberAnswer{UID: uid, Member: false})
			continue
		}
		r := role
		answers = append(answers, verifyMemberAnswer{UID: uid, Member: true, Role: &r})
	}
	c.Response(verifyResponse{ProjectID: projectID, MemberEpoch: epoch, Members: answers})
}

// logLookupFailure reports a store failure, separating the one failure mode an
// operator has to ACT on from the ordinary ones.
//
// ErrLiveProjectOnAbsentSentinel is not a database problem: it means an active
// project is carrying the value the integration contract reserves for "does not
// exist", which only happens while a not-yet-upgraded pod or a rolled-back
// binary is still inserting at the column default. The endpoint refuses rather
// than serving it (see pkg/project.ProjectEpochsInSpace), and the reconcile scan
// repairs the row on its next rotation — but if the scan is disabled, this line
// is the only thing telling the operator why the peer is getting 500s. It names
// the project id, which the wrapped error carries.
//
// Both cases answer the same 500 on the wire. The caller must not be able to
// tell a data anomaly from a database outage.
func (m *Module) logLookupFailure(op string, err error, spaceID string, count int, extra ...zap.Field) {
	fields := append([]zap.Field{
		zap.Error(err), zap.String("space_id", spaceID), zap.Int("count", count),
	}, extra...)
	if errors.Is(err, projectpkg.ErrLiveProjectOnAbsentSentinel) {
		m.Error(op+": refused — an ACTIVE project holds the contract's absent-epoch sentinel; "+
			"serving it would let a consumer cache a grant that never expires. This clears itself "+
			"once the project reconcile scan repairs the row; if that loop is disabled, enable it.",
			fields...)
		return
	}
	m.Error(op+": lookup failed", fields...)
}

// decodeVerifyRequest reads and strictly parses the JSON body: bounded size,
// DisallowUnknownFields so a misspelled field cannot silently be ignored, and
// rejection of trailing garbage.
//
// DisallowUnknownFields is load-bearing on an authorization endpoint: a caller
// that sends `uid` instead of `uids` should get a 400, not a successful 200 with
// an empty answer set that reads as "nobody is a member".
//
// # Neither read may wait on the socket without a bound
//
// This route is served by a zero-value http.Server, so there is no ReadTimeout;
// MaxBytesReader bounds BYTES, not time; and the strict limiter caps arrival
// rate, not the concurrency of requests that never complete. A peer or proxy
// that stalls mid-body — paged out, buffering, or declaring a Content-Length it
// never fills — therefore parks a handler goroutine and a connection for as long
// as it holds the socket. No malice required, and on the authorization path,
// behind a single shared token whose whole threat model is bounding what one
// leaked credential can do.
//
// Two separate reads can wait, and each needs its own answer:
//
//   - The TRAILING check. The obvious way to reject a second value — a second
//     decoder.Decode — cannot return without reading at least one more byte,
//     because that is the only way to tell "another value" from "end of input".
//     This repository adjudicated that hazard once already (PR #837 P1, see the
//     "Do not restore it" note in modules/bot_api/register.go) and solved it in
//     modules/bot_task: inspect what the decoder ALREADY buffered.
//     decoder.Buffered never touches the socket. Nothing is traded away —
//     rejection of trailing content is identical; only the availability
//     behaviour differs. What it CANNOT see is a tail that arrives in a later
//     TCP segment, which is accepted; that is the deliberate half of the trade,
//     and the tests cannot exercise it because an in-memory body is always
//     already buffered.
//   - The FIRST decode, on an INCOMPLETE body. `{"space_id":"a` then silence
//     parks it just as long, and decoder.Buffered does nothing about that. It is
//     bounded by a read deadline installed as the first handler on BOTH routes
//     (boundBodyReadTime), not from inside this function — a deadline set here
//     would miss every request that aborts at the limiter or at auth, which is
//     the half an unauthenticated caller can reach.
func decodeVerifyRequest(c *wkhttp.Context) (verifyRequest, error) {
	// The DECLARED length, before reading anything. MaxBytesReader bounds bytes
	// READ, so a caller may declare far more than the cap, send a small complete
	// object first, and have Decode succeed with nothing left over — the 16 KiB
	// limit never engages, and the oversized remainder is what the post-handler
	// drain then has to deal with. Refusing up front costs one comparison.
	if c.Request.ContentLength > maxRequestBodyBytes {
		return verifyRequest{}, errors.New("verify request declares a body over the limit")
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var req verifyRequest
	if err := decoder.Decode(&req); err != nil {
		return verifyRequest{}, err
	}
	trailing, err := io.ReadAll(decoder.Buffered())
	if err != nil {
		return verifyRequest{}, err
	}
	if len(bytes.TrimSpace(trailing)) > 0 {
		return verifyRequest{}, errors.New("verify request contains trailing data")
	}
	return req, nil
}

// boundBodyReadTime puts a wall clock on reading a request's body.
//
// # It is the FIRST handler on both routes, and that position is the point
//
// An earlier version called this from inside decodeVerifyRequest, which meant the
// deadline was only ever installed for requests that REACHED the handler — i.e.
// after the limiter and after auth. Everything that aborts before that got none,
// and the GET route got none at all. That gap is reachable and it is the more
// dangerous half, because the caller does not need a token to use it:
//
// net/http marks every server request body doEarlyClose, and after the handler
// returns, finishRequest closes it — which, for a declared remainder under
// 256 KiB, drains it with a plain blocking read (net/http/transfer.go's
// io.CopyN into io.Discard). With no ReadTimeout on the server that read has no
// bound. So an unauthenticated caller can POST a Content-Length it never fills,
// take its 401, and still pin a goroutine and a connection for as long as it
// holds the socket. The strict IP bucket does not help: a 429 takes the same
// drain path, which is exactly the "caps arrival rate, not the concurrency of
// requests that never complete" distinction drawn on decodeVerifyRequest.
//
// Installed first, the deadline is already on the connection when that drain
// runs, so the abort paths are covered too. It does not leak into the next
// keep-alive request: net/http re-sets the per-request read deadline from
// ReadTimeout, which is zero here, and that clears it.
//
// # Best-effort by construction
//
// SetReadDeadline reaches the connection through the ResponseWriter chain, and a
// writer that does not support it — an httptest.ResponseRecorder, or a future
// middleware that wraps without Unwrap — returns ErrNotSupported. That is
// ignored rather than failed on: the deadline bounds a hazard, it is not a
// correctness precondition, and refusing real requests because a wrapper lacks a
// method would be the worse failure. gin's own responseWriter does implement
// Unwrap, so it resolves in production.
//
// The value is generous on purpose. maxRequestBodyBytes is 16 KiB, so any real
// peer finishes in milliseconds; readBodyTimeout is not a latency budget, it is
// the line past which a connection is stalled rather than slow.
func boundBodyReadTime() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if c.Request != nil && c.Writer != nil {
			_ = http.NewResponseController(c.Writer).SetReadDeadline(time.Now().Add(readBodyTimeout))
		}
		c.Next()
	}
}
