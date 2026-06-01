package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// ---------- bot model ----------

type botModel struct {
	Id          int64
	SpaceID     string  `db:"space_id"`
	OwnerUID    string  `db:"owner_uid"`
	RuntimeID   int64   `db:"runtime_id"`
	RuntimeKind string  `db:"runtime_kind"`
	DaemonID    string  `db:"daemon_id"`
	Name        string  `db:"name"`
	BotUID      string  `db:"bot_uid"`
	BotToken    string  `db:"bot_token"`
	WorkspaceID string  `db:"workspace_id"`
	Status      string  `db:"status"`
	ClaimToken  string  `db:"claim_token"`
	ErrorMsg    string  `db:"error_msg"`
	CreatedBy   string  `db:"created_by"`
	CreatedAt   db.Time `db:"created_at"`
	UpdatedAt   db.Time `db:"updated_at"`
}

const botSelectColumns = "id, space_id, owner_uid, runtime_id, runtime_kind, daemon_id, name, bot_uid, bot_token, workspace_id, status, claim_token, error_msg, created_by, created_at, updated_at"

const (
	botStatusProvisioning = "provisioning"
	botStatusBotMinted    = "bot_minted"
	botStatusDispatched   = "dispatched"
	botStatusActive       = "active"
	botStatusFailed       = "failed"
	botStatusArchived     = "archived"
)

// Runtime kinds we accept for bot creation. Only "openclaw" actually runs
// tasks in PoC4 — the others are inert (registered but dispatch fails with
// "runtime not supported yet"). See spec §"non-openclaw bot create flow".
const (
	runtimeKindOpenclaw = "openclaw"
	runtimeKindClaude   = "claude"
	runtimeKindCodex    = "codex"
	runtimeKindHermes   = "hermes"
)

func isValidRuntimeKind(k string) bool {
	switch k {
	case runtimeKindOpenclaw, runtimeKindClaude, runtimeKindCodex, runtimeKindHermes:
		return true
	}
	return false
}

// ---------- request / response ----------

type createBotReq struct {
	RuntimeID   int64  `json:"runtime_id"`
	Name        string `json:"name"`
	RuntimeKind string `json:"runtime_kind"`
}

type botResp struct {
	ID          int64  `json:"id"`
	SpaceID     string `json:"space_id"`
	OwnerUID    string `json:"owner_uid"`
	RuntimeID   int64  `json:"runtime_id"`
	RuntimeKind string `json:"runtime_kind"`
	DaemonID    string `json:"daemon_id"`
	Name        string `json:"name"`
	BotUID      string `json:"bot_uid"`
	WorkspaceID string `json:"workspace_id"`
	Status      string `json:"status"`
	ErrorMsg    string `json:"error_msg,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type ackBotReq struct {
	ClaimToken string `json:"claim_token"`
	Status     string `json:"status"`
	ErrorMsg   string `json:"error_msg,omitempty"`
}

func toBotResp(m *botModel) botResp {
	return botResp{
		ID:          m.Id,
		SpaceID:     m.SpaceID,
		OwnerUID:    m.OwnerUID,
		RuntimeID:   m.RuntimeID,
		RuntimeKind: m.RuntimeKind,
		DaemonID:    m.DaemonID,
		Name:        m.Name,
		BotUID:      m.BotUID,
		WorkspaceID: m.WorkspaceID,
		Status:      m.Status,
		ErrorMsg:    m.ErrorMsg,
		CreatedAt:   formatTime(time.Time(m.CreatedAt)),
		UpdatedAt:   formatTime(time.Time(m.UpdatedAt)),
	}
}

// ---------- db helpers ----------

func (d *runtimeDB) insertBot(m *botModel) (int64, error) {
	res, err := d.session.InsertBySql(
		`INSERT INTO bot (space_id, owner_uid, runtime_id, runtime_kind, daemon_id,
		                  name, bot_uid, bot_token, workspace_id, status, error_msg, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'poc')`,
		m.SpaceID, m.OwnerUID, m.RuntimeID, m.RuntimeKind, m.DaemonID,
		m.Name, m.BotUID, m.BotToken, m.WorkspaceID, m.Status, m.ErrorMsg,
	).Exec()
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *runtimeDB) updateBotStatus(id int64, status, errMsg string) error {
	_, err := d.session.UpdateBySql(
		`UPDATE bot SET status=?, error_msg=? WHERE id=?`,
		status, errMsg, id,
	).Exec()
	return err
}

func (d *runtimeDB) queryBotByID(id int64) (*botModel, error) {
	var m botModel
	count, err := d.session.SelectBySql(
		"SELECT "+botSelectColumns+" FROM bot WHERE id=?", id,
	).Load(&m)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return &m, nil
}

func (d *runtimeDB) listBotsBySpace(spaceID, ownerUID, runtimeKind string) ([]*botModel, error) {
	q := d.session.SelectBySql(
		"SELECT "+botSelectColumns+` FROM bot
		 WHERE space_id=? AND status != ?
		   AND (?='' OR owner_uid=?)
		   AND (?='' OR runtime_kind=?)
		 ORDER BY id DESC LIMIT 200`,
		spaceID, botStatusArchived,
		ownerUID, ownerUID,
		runtimeKind, runtimeKind,
	)
	var out []*botModel
	if _, err := q.Load(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// claimPendingBotProvision picks one bot_minted openclaw row for this
// daemon, marks dispatched + sets claim_token, returns it.
func (d *runtimeDB) claimPendingBotProvision(daemonID string) (*botModel, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	var m botModel
	count, err := tx.SelectBySql(
		"SELECT "+botSelectColumns+` FROM bot
		 WHERE daemon_id=? AND runtime_kind=? AND status=?
		 ORDER BY id ASC LIMIT 1 FOR UPDATE`,
		daemonID, runtimeKindOpenclaw, botStatusBotMinted,
	).Load(&m)
	if err != nil || count == 0 {
		return nil, err
	}
	token := randomToken()
	if _, err := tx.UpdateBySql(
		`UPDATE bot SET status=?, claim_token=? WHERE id=?`,
		botStatusDispatched, token, m.Id,
	).Exec(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.Status = botStatusDispatched
	m.ClaimToken = token
	return &m, nil
}

// resolveBotByUID looks up a bot by its bot_uid (called by bot_task
// dispatch to find workspace_id + daemon_id + runtime_kind for a matter
// assignee). Replaces the old managed_runtime_agent reverse lookup.
func (d *runtimeDB) resolveBotByUID(spaceID, botUID string) (*botModel, error) {
	var m botModel
	count, err := d.session.SelectBySql(
		"SELECT "+botSelectColumns+` FROM bot
		 WHERE space_id=? AND bot_uid=? AND status!=?
		 ORDER BY id DESC LIMIT 1`,
		spaceID, botUID, botStatusArchived,
	).Load(&m)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return &m, nil
}

// ---------- helpers ----------

var workspaceSanitizer = regexp.MustCompile(`[^a-z0-9_-]+`)

// deriveWorkspaceID turns the user's bot name into an openclaw workspace
// slug. We always append a short random suffix so two bots named "dev"
// don't collide on the daemon. Workspace is internal — user never sees it.
func deriveWorkspaceID(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = workspaceSanitizer.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "bot"
	}
	if len(s) > 28 {
		s = s[:28]
	}
	suf := make([]byte, 2)
	_, _ = rand.Read(suf)
	return s + "-" + hex.EncodeToString(suf)
}

// ---------- HTTP handlers ----------

// POST /v1/runtimes/bots
// auth = Web user session
func (rt *Runtime) createBot(c *wkhttp.Context) {
	var req createBotReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.RuntimeID <= 0 {
		c.ResponseError(errors.New("runtime_id required"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.ResponseError(errors.New("name required"))
		return
	}
	if !isValidRuntimeKind(req.RuntimeKind) {
		c.ResponseError(fmt.Errorf("runtime_kind must be openclaw|claude|codex|hermes, got %q", req.RuntimeKind))
		return
	}

	loginUID := c.GetLoginUID()
	runtime, err := rt.db.queryByID(req.RuntimeID)
	if err != nil || runtime == nil {
		c.ResponseError(errors.New("runtime not found"))
		return
	}
	if runtime.OwnerUID != loginUID {
		c.ResponseErrorWithStatus(errors.New("no permission to create bot on this runtime"), http.StatusForbidden)
		return
	}
	if runtime.Provider != req.RuntimeKind {
		c.ResponseError(fmt.Errorf("runtime_kind %s does not match runtime provider %s", req.RuntimeKind, runtime.Provider))
		return
	}

	row := &botModel{
		SpaceID:     runtime.SpaceID,
		OwnerUID:    loginUID,
		RuntimeID:   req.RuntimeID,
		RuntimeKind: req.RuntimeKind,
		DaemonID:    runtime.DaemonID,
		Name:        name,
		Status:      botStatusProvisioning,
	}
	if req.RuntimeKind == runtimeKindOpenclaw {
		row.WorkspaceID = deriveWorkspaceID(name)
	}

	id, err := rt.db.insertBot(row)
	if err != nil {
		rt.Error("insert bot", zap.Error(err))
		c.ResponseError(errors.New("create failed"))
		return
	}
	row.Id = id

	botToken := generateBotToken()
	mint, err := botfather.MintBotOBO(rt.ctx, loginUID, runtime.SpaceID, name, botToken)
	if err != nil {
		rt.Error("MintBotOBO", zap.Error(err), zap.Int64("bot_id", id))
		_ = rt.db.updateBotStatus(id, botStatusFailed, fmt.Sprintf("mint bot: %v", err))
		c.ResponseError(fmt.Errorf("mint bot failed: %v", err))
		return
	}
	row.BotUID = mint.BotUID
	row.BotToken = mint.BotToken

	// Decide final status based on runtime_kind. openclaw needs daemon to
	// finish provisioning; others go straight to active (inert).
	var nextStatus string
	if req.RuntimeKind == runtimeKindOpenclaw {
		nextStatus = botStatusBotMinted
	} else {
		nextStatus = botStatusActive
	}
	if _, err := rt.db.session.UpdateBySql(
		`UPDATE bot SET bot_uid=?, bot_token=?, status=? WHERE id=?`,
		mint.BotUID, mint.BotToken, nextStatus, id,
	).Exec(); err != nil {
		rt.Error("post-mint update", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("post-mint update failed"))
		return
	}
	row.Status = nextStatus
	row.CreatedAt = db.Time(time.Now())
	row.UpdatedAt = row.CreatedAt
	c.Response(toBotResp(row))
}

// GET /v1/runtimes/bots?space_id=...&runtime_kind=...&owner_uid=...
// auth = Web user session
func (rt *Runtime) listBots(c *wkhttp.Context) {
	spaceID := c.Query("space_id")
	if spaceID == "" {
		c.ResponseError(errors.New("space_id required"))
		return
	}
	loginUID := c.GetLoginUID()
	var memberCount int
	if err := rt.db.session.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		spaceID, loginUID,
	).LoadOne(&memberCount); err != nil || memberCount == 0 {
		c.ResponseErrorWithStatus(errors.New("not a space member"), http.StatusForbidden)
		return
	}
	owner := c.Query("owner_uid")
	if owner == "me" {
		owner = loginUID
	}
	kind := c.Query("runtime_kind")

	rows, err := rt.db.listBotsBySpace(spaceID, owner, kind)
	if err != nil {
		rt.Error("listBots", zap.Error(err))
		c.ResponseError(errors.New("list failed"))
		return
	}
	out := make([]botResp, 0, len(rows))
	for _, r := range rows {
		out = append(out, toBotResp(r))
	}
	c.Response(gin.H{"bots": out})
}

// GET /v1/runtimes/bots/:id
// auth = Web user session
func (rt *Runtime) getBot(c *wkhttp.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.ResponseError(errors.New("invalid id"))
		return
	}
	m, err := rt.db.queryBotByID(id)
	if err != nil {
		rt.Error("getBot query", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
		return
	}
	if m == nil {
		c.ResponseError(errors.New("not found"))
		return
	}
	loginUID := c.GetLoginUID()
	if m.OwnerUID != loginUID {
		c.ResponseErrorWithStatus(errors.New("no permission"), http.StatusForbidden)
		return
	}
	c.Response(toBotResp(m))
}

// DELETE /v1/runtimes/bots/:id  (soft-delete: status=archived)
// auth = Web user session
func (rt *Runtime) archiveBot(c *wkhttp.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.ResponseError(errors.New("invalid id"))
		return
	}
	m, err := rt.db.queryBotByID(id)
	if err != nil || m == nil {
		c.ResponseError(errors.New("not found"))
		return
	}
	loginUID := c.GetLoginUID()
	if m.OwnerUID != loginUID {
		c.ResponseErrorWithStatus(errors.New("no permission"), http.StatusForbidden)
		return
	}
	if err := rt.db.updateBotStatus(id, botStatusArchived, ""); err != nil {
		rt.Error("archiveBot", zap.Error(err))
		c.ResponseError(errors.New("archive failed"))
		return
	}
	c.ResponseOK()
}

// POST /v1/daemon/bots/:id/ack
// auth = daemon API key
func (rt *Runtime) ackBot(c *wkhttp.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.ResponseError(errors.New("invalid id"))
		return
	}
	var req ackBotReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid body"))
		return
	}
	if req.Status != botStatusActive && req.Status != botStatusFailed {
		c.ResponseError(errors.New("status must be active or failed"))
		return
	}
	m, err := rt.db.queryBotByID(id)
	if err != nil || m == nil {
		c.ResponseError(errors.New("bot not found"))
		return
	}
	spaceID := c.MustGet("space_id").(string)
	if m.SpaceID != spaceID {
		c.ResponseErrorWithStatus(errors.New("no permission"), http.StatusForbidden)
		return
	}
	if m.ClaimToken == "" || m.ClaimToken != req.ClaimToken {
		c.ResponseErrorWithStatus(errors.New("invalid or stale claim_token"), http.StatusConflict)
		return
	}
	if err := rt.db.updateBotStatus(id, req.Status, req.ErrorMsg); err != nil {
		rt.Error("ackBot update", zap.Error(err))
		c.ResponseError(errors.New("ack failed"))
		return
	}
	c.ResponseOK()
}

// buildPendingBotProvision renders the heartbeat payload for daemon.
func (rt *Runtime) buildPendingBotProvision(m *botModel) gin.H {
	cfg := rt.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}
	return gin.H{
		"id":           m.Id,
		"action":       "bot.provision",
		"workspace_id": m.WorkspaceID,
		"display_name": m.Name,
		"bot_uid":      m.BotUID,
		"bot_token":    m.BotToken,
		"api_url":      apiURL,
		"claim_token":  m.ClaimToken,
	}
}

// ---------- shared helpers (moved here from deleted managed_agent.go) ----------

// randomToken: 16-byte hex string used as bot_task / bot ack claim token.
func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// generateBotToken mirrors the bf_xxx scheme the IM /newbot flow uses,
// so downstream Octo /v1/bot/* endpoints don't need a special-case parser.
func generateBotToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "bf_" + hex.EncodeToString(b)
}
