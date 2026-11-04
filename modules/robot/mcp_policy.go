package robot

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/mcppolicy"
	"github.com/gocraft/dbr/v2"
)

var (
	errMCPRobotNotFound  = errors.New("mcp: robot not found")
	errMCPRobotForbidden = errors.New("mcp: creator required")
)

// Reuse the exact active-robot/creator semantics of assertRobotOwner. The
// bounded query and fixed fault class prevent policy failures logging inputs.
func mcpRobotOwner(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, uid, actor string, lock bool) error {
	if !mcppolicy.ValidUID(uid) {
		return errMCPRobotNotFound
	}
	q := "SELECT IFNULL(creator_uid,'') FROM robot WHERE robot_id=? AND BINARY robot_id=BINARY ? AND status=1"
	if lock {
		q += " FOR UPDATE"
	}
	var creator string
	err := query.QueryRowContext(ctx, q, uid, uid).Scan(&creator)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mcppolicy.ErrUnavailable
	}
	switch decideOwnership(creator, actor) {
	case ownershipNotFound:
		return errMCPRobotNotFound
	case ownershipForbidden:
		return errMCPRobotForbidden
	default:
		return nil
	}
}

func (rb *Robot) respondMCPPolicyError(c *wkhttp.Context, err error) {
	switch {
	case errors.Is(err, errMCPRobotNotFound):
		httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotNotFound, nil, nil)
	case errors.Is(err, errMCPRobotForbidden):
		httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotCreatorOnly, nil, nil)
	case errors.Is(err, mcppolicy.ErrInvalid):
		httperr.ResponseErrorL(c, errcode.ErrMCPPolicyInvalid, nil, nil)
	case errors.Is(err, mcppolicy.ErrConflict):
		httperr.ResponseErrorL(c, errcode.ErrMCPPolicyConflict, nil, nil)
	default:
		rb.Error("mcp policy unavailable")
		httperr.ResponseErrorL(c, errcode.ErrMCPPolicyUnavailable, nil, nil)
	}
}

func (rb *Robot) checkMCPPolicyOwner(c *wkhttp.Context) bool {
	c.Header("Cache-Control", "private, no-store")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	err := mcpRobotOwner(ctx, rb.ctx.DB().DB, c.Param("robot_id"), c.GetLoginUID(), false)
	if err != nil {
		rb.respondMCPPolicyError(c, err)
		return false
	}
	return true
}

func (rb *Robot) getMCPPolicy(c *wkhttp.Context) {
	if !rb.checkMCPPolicyOwner(c) {
		return
	}
	uid := c.Param("robot_id")
	p, err := (mcppolicy.Store{Session: rb.ctx.DB()}).Resolve(c.Request.Context(), uid)
	if err != nil {
		rb.respondMCPPolicyError(c, err)
		return
	}
	c.Response(mcppolicy.BotPolicy{BotUID: uid, Policy: p})
}

func (rb *Robot) putMCPPolicy(c *wkhttp.Context)    { rb.mutateMCPPolicy(c, false) }
func (rb *Robot) deleteMCPPolicy(c *wkhttp.Context) { rb.mutateMCPPolicy(c, true) }

func (rb *Robot) mutateMCPPolicy(c *wkhttp.Context, deleteOverride bool) {
	// Authenticate and check ownership before exposing field-validation errors.
	if !rb.checkMCPPolicyOwner(c) {
		return
	}
	req, err := mcppolicy.ParseMutation(c.Request.Body, deleteOverride)
	if err != nil {
		rb.respondMCPPolicyError(c, err)
		return
	}
	uid, actor := c.Param("robot_id"), c.GetLoginUID()
	check := func(ctx context.Context, tx *dbr.Tx) error {
		return mcpRobotOwner(ctx, tx, uid, actor, true)
	}
	store := mcppolicy.Store{Session: rb.ctx.DB()}
	var p mcppolicy.Policy
	if deleteOverride {
		p, err = store.DeleteBot(c.Request.Context(), uid, actor, req.Generation, check)
	} else {
		p, err = store.PutBot(c.Request.Context(), uid, actor, req.Mode, req.Generation, check)
	}
	if err != nil {
		rb.respondMCPPolicyError(c, err)
		return
	}
	c.Response(mcppolicy.BotPolicy{BotUID: uid, Policy: p})
}
