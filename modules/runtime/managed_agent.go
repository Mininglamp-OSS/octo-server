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

// ---------- model ----------

type managedAgentModel struct {
	Id          int64
	AgentID     string  `db:"agent_id"`
	SpaceID     string  `db:"space_id"`
	OwnerUID    string  `db:"owner_uid"`
	RuntimeID   int64   `db:"runtime_id"`
	DaemonID    string  `db:"daemon_id"`
	DisplayName string  `db:"display_name"`
	Provider    string  `db:"provider"`
	BotUID      string  `db:"bot_uid"`
	BotToken    string  `db:"bot_token"`
	Status      string  `db:"status"`
	CommandKind string  `db:"command_kind"` // "agent.create" | "bot.add"
	// dispatched_at is omitted from the model: it's a NULL datetime which
	// db.Time can't scan, and the field is only used SQL-side via NOW().
	ClaimToken string  `db:"claim_token"`
	ErrorMsg   string  `db:"error_msg"`
	CreatedBy  string  `db:"created_by"`
	CreatedAt  db.Time `db:"created_at"`
	UpdatedAt  db.Time `db:"updated_at"`
}

// selectColumns lists every column managedAgentModel cares about, so dbr's
// SELECT does not pull dispatched_at (NULL-able datetime, can't scan into db.Time).
const managedAgentSelectColumns = "id, agent_id, space_id, owner_uid, runtime_id, daemon_id, display_name, provider, bot_uid, bot_token, status, command_kind, claim_token, error_msg, created_by, created_at, updated_at"

const (
	maStatusProvisioning = "provisioning"
	maStatusBotMinted    = "bot_minted"
	maStatusDispatched   = "dispatched"
	maStatusActive       = "active"
	maStatusFailed       = "failed"

	maKindAgentCreate = "agent.create"
	maKindBotAdd      = "bot.add"
)

// ---------- request / response ----------

type createManagedAgentReq struct {
	RuntimeID   int64  `json:"runtime_id"`             // required: openclaw runtime to attach to
	DisplayName string `json:"display_name"`           // required: shown in UI & used as bot name
	AgentID     string `json:"agent_id,omitempty"`     // optional: openclaw agent id slug; derived if empty
}

type managedAgentResp struct {
	ID          int64  `json:"id"`
	AgentID     string `json:"agent_id"`
	SpaceID     string `json:"space_id"`
	RuntimeID   int64  `json:"runtime_id"`
	DaemonID    string `json:"daemon_id"`
	DisplayName string `json:"display_name"`
	Provider    string `json:"provider"`
	BotUID      string `json:"bot_uid"`
	Status      string `json:"status"`
	ErrorMsg    string `json:"error_msg,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type ackManagedAgentReq struct {
	ClaimToken string `json:"claim_token"`
	Status     string `json:"status"` // "active" or "failed"
	ErrorMsg   string `json:"error_msg,omitempty"`
}

func toManagedAgentResp(m *managedAgentModel) managedAgentResp {
	return managedAgentResp{
		ID:          m.Id,
		AgentID:     m.AgentID,
		SpaceID:     m.SpaceID,
		RuntimeID:   m.RuntimeID,
		DaemonID:    m.DaemonID,
		DisplayName: m.DisplayName,
		Provider:    m.Provider,
		BotUID:      m.BotUID,
		Status:      m.Status,
		ErrorMsg:    m.ErrorMsg,
		CreatedAt:   formatTime(time.Time(m.CreatedAt)),
		UpdatedAt:   formatTime(time.Time(m.UpdatedAt)),
	}
}

// ---------- db helpers (methods on runtimeDB) ----------

func (d *runtimeDB) insertManagedAgent(m *managedAgentModel) (int64, error) {
	kind := m.CommandKind
	if kind == "" {
		kind = maKindAgentCreate
	}
	res, err := d.session.InsertBySql(
		`INSERT INTO managed_runtime_agent
		   (agent_id, space_id, owner_uid, runtime_id, daemon_id, display_name, provider, status, command_kind, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'poc')`,
		m.AgentID, m.SpaceID, m.OwnerUID, m.RuntimeID, m.DaemonID, m.DisplayName, m.Provider, m.Status, kind,
	).Exec()
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *runtimeDB) updateManagedAgentBot(id int64, botUID, botToken string) error {
	_, err := d.session.UpdateBySql(
		`UPDATE managed_runtime_agent SET bot_uid=?, bot_token=?, status=? WHERE id=?`,
		botUID, botToken, maStatusBotMinted, id,
	).Exec()
	return err
}

func (d *runtimeDB) updateManagedAgentStatus(id int64, status, errMsg string) error {
	_, err := d.session.UpdateBySql(
		`UPDATE managed_runtime_agent SET status=?, error_msg=? WHERE id=?`,
		status, errMsg, id,
	).Exec()
	return err
}

func (d *runtimeDB) queryManagedAgentByID(id int64) (*managedAgentModel, error) {
	var m managedAgentModel
	count, err := d.session.SelectBySql(
		"SELECT "+managedAgentSelectColumns+" FROM managed_runtime_agent WHERE id=?", id,
	).Load(&m)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return &m, nil
}

// claimPendingManagedAgentCommand atomically picks one bot_minted row for this
// daemon, marks it dispatched with a fresh claim_token, and returns it. The
// claim_token is what the daemon must echo back in ack to prove it really got
// this dispatch (prevents stale ack from a previous attempt).
func (d *runtimeDB) claimPendingManagedAgentCommand(daemonID string) (*managedAgentModel, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	var m managedAgentModel
	count, err := tx.SelectBySql(
		"SELECT "+managedAgentSelectColumns+` FROM managed_runtime_agent
		 WHERE daemon_id=? AND status=?
		 ORDER BY id ASC LIMIT 1 FOR UPDATE`,
		daemonID, maStatusBotMinted,
	).Load(&m)
	if err != nil || count == 0 {
		return nil, err
	}

	token := randomToken()
	_, err = tx.UpdateBySql(
		`UPDATE managed_runtime_agent
		   SET status=?, dispatched_at=NOW(), claim_token=?
		 WHERE id=?`,
		maStatusDispatched, token, m.Id,
	).Exec()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.Status = maStatusDispatched
	m.ClaimToken = token
	return &m, nil
}

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

var agentIDSanitizer = regexp.MustCompile(`[^a-z0-9_-]+`)

func deriveAgentID(displayName string) string {
	s := strings.ToLower(strings.TrimSpace(displayName))
	s = agentIDSanitizer.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "agent"
	}
	if len(s) > 32 {
		s = s[:32]
	}
	// suffix to avoid collisions: 4 hex chars from rand
	suf := make([]byte, 2)
	_, _ = rand.Read(suf)
	return s + "-" + hex.EncodeToString(suf)
}

// ---------- HTTP handlers ----------

// POST /v1/runtimes/:runtime_id/agents/:agent_id/bots
// auth = Web user session
//
// Mints a new bot and queues a `bot.add` command for the daemon to bind it
// to an existing openclaw agent (does NOT create a new agent workspace).
// Caller supplies the openclaw agent_id (the slug shown in `openclaw agents
// list`, e.g. "main", "cc", or a previously-PoC-created id).
//
// We don't validate that the agent_id actually exists on the daemon — daemon
// `openclaw agents bind --agent <id>` will fail and surface via ack status=failed
// if the agent_id is unknown. PoC trade-off: keeps the server stateless wrt
// daemon agent inventory.
func (rt *Runtime) addBotToManagedAgent(c *wkhttp.Context) {
	runtimeIDStr := c.Param("runtime_id")
	runtimeID, err := strconv.ParseInt(runtimeIDStr, 10, 64)
	if err != nil {
		c.ResponseError(errors.New("invalid runtime_id"))
		return
	}
	agentID := strings.TrimSpace(c.Param("agent_id"))
	if agentID == "" {
		c.ResponseError(errors.New("agent_id is required"))
		return
	}

	var req createManagedAgentReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		// fallback so the bot has a name; user can rename via BotFather later.
		displayName = agentID + " bot"
	}

	loginUID := c.GetLoginUID()

	runtime, err := rt.db.queryByID(runtimeID)
	if err != nil || runtime == nil {
		c.ResponseError(errors.New("runtime not found"))
		return
	}
	if runtime.OwnerUID != loginUID {
		c.ResponseErrorWithStatus(errors.New("no permission to add bot on this runtime"), http.StatusForbidden)
		return
	}
	if runtime.Provider != "openclaw" {
		c.ResponseError(fmt.Errorf("bot bind only supported on provider=openclaw, got %s", runtime.Provider))
		return
	}

	row := &managedAgentModel{
		AgentID:     agentID, // reuse existing openclaw agent_id (NOT derived)
		SpaceID:     runtime.SpaceID,
		OwnerUID:    loginUID,
		RuntimeID:   runtimeID,
		DaemonID:    runtime.DaemonID,
		DisplayName: displayName,
		Provider:    "openclaw",
		Status:      maStatusProvisioning,
		CommandKind: maKindBotAdd,
	}
	id, err := rt.db.insertManagedAgent(row)
	if err != nil {
		rt.Error("insert managed_runtime_agent (bot.add)", zap.Error(err))
		c.ResponseError(errors.New("create failed"))
		return
	}
	row.Id = id

	botToken := generateBotToken()
	mint, err := botfather.MintBotOBO(rt.ctx, loginUID, runtime.SpaceID, displayName, botToken)
	if err != nil {
		rt.Error("MintBotOBO (bot.add) failed", zap.Error(err), zap.Int64("managed_agent_id", id))
		_ = rt.db.updateManagedAgentStatus(id, maStatusFailed, fmt.Sprintf("mint bot: %v", err))
		c.ResponseError(fmt.Errorf("mint bot failed: %v", err))
		return
	}

	if err := rt.db.updateManagedAgentBot(id, mint.BotUID, mint.BotToken); err != nil {
		rt.Error("update managed_runtime_agent bot fields (bot.add)", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("post-mint update failed"))
		return
	}

	row.BotUID = mint.BotUID
	row.BotToken = mint.BotToken
	row.Status = maStatusBotMinted
	row.CreatedAt = db.Time(time.Now())
	row.UpdatedAt = db.Time(time.Now())
	c.Response(toManagedAgentResp(row))
}

// POST /v1/runtimes/managed-agents
// auth = Web user session (AuthMiddleware)
//
// PoC flow (synchronous bot mint, async openclaw provision via heartbeat):
//   1. validate input & runtime ownership
//   2. insert row (status=provisioning)
//   3. mint bot via botfather.MintBotOBO → status=bot_minted
//   4. return 200 with current row
//   (daemon picks up via next heartbeat → dispatches openclaw command → ack → active)
func (rt *Runtime) createManagedAgent(c *wkhttp.Context) {
	var req createManagedAgentReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.RuntimeID <= 0 {
		c.ResponseError(errors.New("runtime_id is required"))
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		c.ResponseError(errors.New("display_name is required"))
		return
	}

	loginUID := c.GetLoginUID()

	runtime, err := rt.db.queryByID(req.RuntimeID)
	if err != nil || runtime == nil {
		c.ResponseError(errors.New("runtime not found"))
		return
	}
	if runtime.OwnerUID != loginUID {
		c.ResponseErrorWithStatus(errors.New("no permission to create agent on this runtime"), http.StatusForbidden)
		return
	}
	if runtime.Provider != "openclaw" {
		c.ResponseError(fmt.Errorf("managed agent only supported on provider=openclaw, got %s", runtime.Provider))
		return
	}

	agentID := strings.TrimSpace(req.AgentID)
	if agentID == "" {
		// Use the display name verbatim as the openclaw agent id. We do NOT
		// derive a random suffix anymore — the user picks the id, and if it
		// collides with an existing agent the daemon-side `openclaw agents
		// add` will fail and surface via ack status=failed.
		agentID = strings.TrimSpace(req.DisplayName)
	}
	if agentID == "" {
		c.ResponseError(errors.New("agent_id (or display_name) required"))
		return
	}

	row := &managedAgentModel{
		AgentID:     agentID,
		SpaceID:     runtime.SpaceID,
		OwnerUID:    loginUID,
		RuntimeID:   req.RuntimeID,
		DaemonID:    runtime.DaemonID,
		DisplayName: req.DisplayName,
		Provider:    "openclaw",
		// Skip the "provisioning → bot_minted" hop: agent.create no longer
		// mints a bot, so the row is immediately ready for daemon claim.
		Status:      maStatusBotMinted,
		CommandKind: maKindAgentCreate,
	}
	id, err := rt.db.insertManagedAgent(row)
	if err != nil {
		rt.Error("insert managed_runtime_agent", zap.Error(err))
		c.ResponseError(errors.New("create failed"))
		return
	}
	row.Id = id
	row.CreatedAt = db.Time(time.Now())
	row.UpdatedAt = db.Time(time.Now())

	// No MintBotOBO here — `agent.create` only creates an openclaw agent
	// workspace. Bots are attached separately via the bot.add endpoint.
	c.Response(toManagedAgentResp(row))
}

// GET /v1/runtimes/managed-agents/:id
// auth = Web user session
func (rt *Runtime) getManagedAgent(c *wkhttp.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.ResponseError(errors.New("invalid id"))
		return
	}
	m, err := rt.db.queryManagedAgentByID(id)
	if err != nil {
		rt.Error("getManagedAgent: query failed", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(fmt.Errorf("query: %v", err))
		return
	}
	if m == nil {
		rt.Warn("getManagedAgent: not found", zap.Int64("id", id))
		c.ResponseError(errors.New("not found"))
		return
	}
	loginUID := c.GetLoginUID()
	if m.OwnerUID != loginUID {
		c.ResponseErrorWithStatus(errors.New("no permission"), http.StatusForbidden)
		return
	}
	c.Response(toManagedAgentResp(m))
}

// POST /v1/daemon/managed-agents/:id/ack
// auth = daemon API key
// body: { claim_token, status: "active"|"failed", error_msg? }
func (rt *Runtime) ackManagedAgent(c *wkhttp.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.ResponseError(errors.New("invalid id"))
		return
	}
	var req ackManagedAgentReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.Status != maStatusActive && req.Status != maStatusFailed {
		c.ResponseError(errors.New("status must be 'active' or 'failed'"))
		return
	}

	m, err := rt.db.queryManagedAgentByID(id)
	if err != nil || m == nil {
		c.ResponseError(errors.New("managed agent not found"))
		return
	}
	ownerUID := c.MustGet("owner_uid").(string)
	spaceID := c.MustGet("space_id").(string)
	if m.OwnerUID != ownerUID || m.SpaceID != spaceID {
		c.ResponseErrorWithStatus(errors.New("no permission"), http.StatusForbidden)
		return
	}
	if m.ClaimToken == "" || m.ClaimToken != req.ClaimToken {
		// stale or replayed ack — refuse silently to avoid clobbering newer state
		c.ResponseErrorWithStatus(errors.New("invalid or stale claim_token"), http.StatusConflict)
		return
	}

	if err := rt.db.updateManagedAgentStatus(id, req.Status, req.ErrorMsg); err != nil {
		rt.Error("ack update", zap.Error(err))
		c.ResponseError(errors.New("ack failed"))
		return
	}
	c.Response(gin.H{"status": "ok"})
}

// buildPendingAgentCommand renders a heartbeat-friendly payload for the daemon.
// External api_url comes from server config; the daemon needs it to bind
// the bot to openclaw channels.octo (account_id + apiUrl pair).
//
// action mirrors row.CommandKind:
//   - "agent.create" → daemon runs `openclaw agents add <id> --bind octo:<bot>`
//   - "bot.add"      → daemon runs `openclaw agents bind --agent <id> --bind octo:<bot>` only
func (rt *Runtime) buildPendingAgentCommand(m *managedAgentModel) gin.H {
	cfg := rt.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}
	action := m.CommandKind
	if action == "" {
		action = maKindAgentCreate
	}
	return gin.H{
		"id":           m.Id,
		"action":       action,
		"agent_id":     m.AgentID,
		"display_name": m.DisplayName,
		"bot_uid":      m.BotUID,
		"bot_token":    m.BotToken,
		"api_url":      apiURL,
		"claim_token":  m.ClaimToken,
	}
}
