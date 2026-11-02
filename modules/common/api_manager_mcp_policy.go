package common

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	"github.com/Mininglamp-OSS/octo-server/pkg/mcppolicy"
)

var errMCPPolicyForbidden = func() codes.Code {
	code, ok := codes.Lookup("err.shared.auth.forbidden")
	if !ok {
		panic("common: shared auth forbidden code missing")
	}
	return code
}()

func (m *Manager) mcpPolicyAdmin(c *wkhttp.Context) bool {
	c.Header("Cache-Control", "private, no-store")
	if c.CheckLoginRoleIsSuperAdmin() != nil {
		httperr.ResponseErrorL(c, errMCPPolicyForbidden, nil, nil)
		return false
	}
	return true
}

func (m *Manager) respondMCPPolicyError(c *wkhttp.Context, err error) {
	switch {
	case errors.Is(err, mcppolicy.ErrInvalid):
		httperr.ResponseErrorL(c, errcode.ErrMCPPolicyInvalid, nil, nil)
	case errors.Is(err, mcppolicy.ErrConflict):
		httperr.ResponseErrorL(c, errcode.ErrMCPPolicyConflict, nil, nil)
	default:
		m.Error("mcp policy unavailable")
		httperr.ResponseErrorL(c, errcode.ErrMCPPolicyUnavailable, nil, nil)
	}
}

func (m *Manager) getMCPPolicyDefault(c *wkhttp.Context) {
	if !m.mcpPolicyAdmin(c) {
		return
	}
	p, err := (mcppolicy.Store{Session: m.ctx.DB()}).GetDefault(c.Request.Context())
	if err != nil {
		m.respondMCPPolicyError(c, err)
		return
	}
	c.Response(p)
}

func (m *Manager) putMCPPolicyDefault(c *wkhttp.Context) {
	if !m.mcpPolicyAdmin(c) {
		return
	}
	req, err := mcppolicy.ParseMutation(c.Request.Body, false)
	if err != nil {
		m.respondMCPPolicyError(c, err)
		return
	}
	p, err := (mcppolicy.Store{Session: m.ctx.DB()}).PutDefault(c.Request.Context(), c.GetLoginUID(), req.Mode, req.Generation)
	if err != nil {
		m.respondMCPPolicyError(c, err)
		return
	}
	c.Response(p)
}
