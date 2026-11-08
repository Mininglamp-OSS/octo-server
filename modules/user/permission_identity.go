package user

import (
	"context"
	"database/sql"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/mcppolicy"
	"github.com/Mininglamp-OSS/octo-server/pkg/permissionbundle"
	"github.com/gocraft/dbr/v2"
)

type managedBotIdentity struct {
	IdentityVersion int               `json:"identity_version"`
	PrincipalKind   string            `json:"principal_kind"`
	BotUID          string            `json:"bot_uid"`
	MCPPolicy       *mcppolicy.Policy `json:"mcp_policy,omitempty"`
}

func (u *User) authVerifyManagedBot(c *wkhttp.Context, req authVerifyBotReq) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	session := *u.db.session
	session.EventReceiver = &dbr.NullEventReceiver{}
	tx, e := session.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if e != nil {
		httperr.ResponseErrorL(c, errcode.ErrMCPPolicyUnavailable, nil, nil)
		return
	}
	defer tx.RollbackUnlessCommitted()
	// Current token/robot row lock; byte equality rejects a collation alias.
	rows, e := tx.QueryContext(ctx, `SELECT robot_id,IFNULL(creator_uid,''),bot_token,status FROM robot WHERE bot_token=? FOR SHARE NOWAIT`, req.BotToken)
	if e != nil {
		httperr.ResponseErrorLWithStatus(c, errSharedAuthRequired, nil, nil)
		return
	}
	var uid, creator, token string
	var status, count int
	for rows.Next() {
		count++
		if rows.Scan(&uid, &creator, &token, &status) != nil {
			count = -1
			break
		}
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil || count != 1 || token != req.BotToken || status != 1 || (permissionbundle.SQLNative{}).Identity(ctx, tx, uid, creator) != nil {
		httperr.ResponseErrorLWithStatus(c, errSharedAuthRequired, nil, nil)
		return
	}
	if tx.Commit() != nil {
		httperr.ResponseErrorL(c, errcode.ErrMCPPolicyUnavailable, nil, nil)
		return
	}
	resp := managedBotIdentity{IdentityVersion: 2, PrincipalKind: "user_bot", BotUID: uid}
	if req.IncludeMCPPolicy {
		p, e := (mcppolicy.Store{Session: u.db.session}).Resolve(ctx, uid)
		if e != nil {
			httperr.ResponseErrorL(c, errcode.ErrMCPPolicyUnavailable, nil, nil)
			return
		}
		resp.MCPPolicy = &p
	}
	c.Response(resp)
}
