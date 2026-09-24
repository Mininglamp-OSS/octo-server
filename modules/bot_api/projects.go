// Package bot_api · Project read API for User Bots.
//
// Two read-only endpoints, and nothing else from modules/project:
//
//	GET /v1/bot/projects                 — list, scoped to one Space (space_id is
//	                                       required; there is no implicit fallback
//	                                       to "the bot's first Space", because a
//	                                       multi-Space bot would then silently read
//	                                       a subset that looks complete)
//	GET /v1/bot/projects/:project_id     — one Project, Space derived from the row
//
// Visibility (confirmed with the owner): a Project is visible to the bot when
// the BOT itself or its OWNER (robot.creator_uid) holds an active
// `octo_project_member` seat in it AND that uid is still a live member of the
// Space. Bots are not addable to Projects as ordinary members in practice — they
// participate as their owner's agent — so the owner's seats carry the
// visibility; the bot's own seats are kept as a strict subset (an org-catalog
// agent that WAS added keeps seeing its Project).
//
// What is deliberately NOT weakened:
//
//   - The bot must hold an active `space_member` seat in the requested Space and
//     that Space must be active — modules/project's own projectReadSpaceAccessTx,
//     the same predicate the user-facing reads use.
//   - The owner's seats do not widen the caller's Space scope, and they only
//     count WHILE the owner is still there: modules/project re-checks every seat
//     uid against that same Space gate, so a revoked or destroyed owner cannot
//     keep a Project readable through their bot. (Needed because seat closure is
//     asynchronous and account destruction closes nothing at all.)
//   - Seat membership is read live (status active AND removing = 0), which is how
//     a seat closes when its holder leaves the Project.
//   - The detail read collapses EVERY invisible case into one not-found
//     (err.server.project.not_found), the same anti-enumeration answer the user
//     API gives. Only the list — where the caller names the Space and there is no
//     resource identity to hide — reports "not a member of this Space".
//
// The response is the module's own Resp projection, so my_role and capabilities
// describe the seat that decided visibility (the bot's own seat first, else the
// owner's). Those advisory flags are not an authorization surface: the bot has
// no Project write endpoint to reach.
//
// Error dialect: bot_api codes for bot-facing conditions (request_invalid /
// not_space_member / app_bot_unsupported / query_failed), plus
// err.server.project.not_found for the Project resource itself. No new codes, so
// no i18n resource change.
package bot_api

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// botListProjects handles GET /v1/bot/projects.
//
// Response is the SAME projection and the SAME pagination contract as the
// user-facing list: an array of Project Resp objects plus X-Total-Count, with
// `keyword` filtering names and `page`/`limit` bounded by modules/project. An
// empty result is `[]`, never null.
func (ba *BotAPI) botListProjects(c *wkhttp.Context) {
	callerUID, seatUIDs, ok := ba.botProjectReadPrincipal(c)
	if !ok {
		return
	}
	spaceID := strings.TrimSpace(c.Query("space_id"))
	if spaceID == "" {
		respondBotAPIRequestInvalid(c, "space_id")
		return
	}
	page, limit := botProjectPageParams(c)
	rows, total, err := projectmod.ReadProjectsForPrincipal(
		ba.ctx, spaceID, callerUID, strings.TrimSpace(c.Query("keyword")), seatUIDs, page, limit,
	)
	if err != nil {
		ba.respondBotProjectReadError(c, err, "list projects")
		return
	}
	c.Writer.Header().Set("X-Total-Count", strconv.FormatInt(total, 10))
	c.Response(rows)
}

// botGetProject handles GET /v1/bot/projects/:project_id.
//
// No space_id parameter: the Space is derived from the Project row inside the
// read transaction, exactly like the user-facing detail read. Accepting a
// client-supplied scope would add a mis-scoping footgun (a correct project id
// plus the wrong Space would read as "not found") with no security benefit.
func (ba *BotAPI) botGetProject(c *wkhttp.Context) {
	callerUID, seatUIDs, ok := ba.botProjectReadPrincipal(c)
	if !ok {
		return
	}
	projectID := strings.TrimSpace(c.Param("project_id"))
	if projectID == "" {
		respondBotAPIRequestInvalid(c, "project_id")
		return
	}
	resp, err := projectmod.ReadProjectForPrincipal(ba.ctx, projectID, callerUID, seatUIDs)
	if err != nil {
		ba.respondBotProjectReadError(c, err, "get project")
		return
	}
	c.Response(resp)
}

// botProjectReadPrincipal resolves the authenticated caller and the ordered set
// of uids whose active Project seats make a Project readable for it.
//
// The owner uid is taken from the *robotModel authBot already loaded for this
// request (CtxKeyRobot), so this costs no extra query. An orphan bot with an
// empty creator_uid degenerates to its own seats.
func (ba *BotAPI) botProjectReadPrincipal(c *wkhttp.Context) (string, []string, bool) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
		return "", nil, false
	}
	if getBotKindFromContext(c) == BotKindApp {
		// DM-scoped like every other non-DM bot surface, and structurally unable
		// to hold a Project seat: app_bot rows are inserted behind a uniqueness
		// check that FORBIDS a same-uid user/robot row
		// (modules/app_bot/db.go:59), while both the Space-seat and the Project-seat
		// predicates read those tables.
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIAppBotUnsupported, nil, nil)
		return "", nil, false
	}
	seatUIDs := []string{robotID}
	if robot := getRobotFromContext(c); robot != nil {
		if owner := strings.TrimSpace(robot.CreatorUID); owner != "" {
			seatUIDs = append(seatUIDs, owner)
		}
	}
	return robotID, seatUIDs, true
}

// respondBotProjectReadError maps the cross-module read outcomes onto the bot
// error dialect. Status-preserving (WithStatus) on purpose: bot adapters branch
// on the real 403/404/500, not on the D14 fixed 400.
func (ba *BotAPI) respondBotProjectReadError(c *wkhttp.Context, err error, entry string) {
	switch {
	case errors.Is(err, projectmod.ErrProjectReadForbidden):
		// The caller named the Space and is not an eligible active member of it.
		// One code for "Space does not exist" and "not my Space", so this cannot
		// be used to probe Space existence.
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPINotSpaceMember, nil, nil)
	case errors.Is(err, projectmod.ErrProjectReadNotFound):
		// Absent, dissolved, another Space, or no seat visible to this principal:
		// one byte-identical answer, as on the user API.
		httperr.ResponseErrorLWithStatus(c, errcode.ErrProjectNotFound, nil, nil)
	default:
		ba.Error("bot project read failed", zap.String("entry", entry), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIQueryFailed, nil, nil)
	}
}

// botProjectPageParams parses page/limit leniently — an unparsable or
// non-positive value falls back to the module default rather than erroring, the
// same posture as GET /v1/bot/space/members. The caps (default 50, max 200, page
// cap 100000) live in modules/project and are applied before the offset is
// computed, so a hostile limit cannot turn into SQL LIMIT -5 → 1064 → 500.
func botProjectPageParams(c *wkhttp.Context) (page, limit int64) {
	page = 1
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 {
			page = value
		}
	}
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
			limit = value
		}
	}
	return page, limit
}
