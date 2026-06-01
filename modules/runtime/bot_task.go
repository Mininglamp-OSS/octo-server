package runtime

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// ---------- bot_task model ----------

// botTaskModel mirrors the bot_task row used to dispatch matter-driven agent
// runs to the daemon. Columns that may be NULL on disk (none today, but kept
// explicit so future ALTERs don't surprise dbr) are listed in
// botTaskSelectColumns, not pulled by SELECT *.
type botTaskModel struct {
	Id            int64
	MatterID      string  `db:"matter_id"`
	MatterBaseURL string  `db:"matter_base_url"`
	SpaceID       string  `db:"space_id"`
	BotUID        string  `db:"bot_uid"`
	AgentID       string  `db:"agent_id"`
	DaemonID      string  `db:"daemon_id"`
	RuntimeID     int64   `db:"runtime_id"`
	RequesterUID  string  `db:"requester_uid"`
	Title         string  `db:"title"`
	Description   string  `db:"description"`
	Prompt        string  `db:"prompt"`
	Status        string  `db:"status"`
	ClaimToken    string  `db:"claim_token"`
	ResultSummary string  `db:"result_summary"`
	ErrorMsg      string  `db:"error_msg"`
	CreatedBy     string  `db:"created_by"`
	CreatedAt     db.Time `db:"created_at"`
	UpdatedAt     db.Time `db:"updated_at"`
}

const botTaskSelectColumns = "id, matter_id, matter_base_url, space_id, bot_uid, agent_id, daemon_id, runtime_id, requester_uid, title, description, prompt, status, claim_token, result_summary, error_msg, created_by, created_at, updated_at"

const (
	btStatusQueued     = "queued"
	btStatusDispatched = "dispatched"
	btStatusSucceeded  = "succeeded"
	btStatusFailed     = "failed"
)

// ---------- request / response ----------

type createBotTaskReq struct {
	MatterID      string `json:"matter_id"`
	MatterBaseURL string `json:"matter_base_url"` // e.g. "http://127.0.0.1:8080"
	SpaceID       string `json:"space_id"`
	BotUID        string `json:"bot_uid"`
	RequesterUID  string `json:"requester_uid"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	// Prompt: optional override. When non-empty replaces the title+description
	// composition (composeBotTaskPrompt). Used by matter @mention dispatch to
	// feed the agent the full conversation history.
	Prompt string `json:"prompt,omitempty"`
}

type botTaskResp struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	AgentID   string `json:"agent_id"`
	DaemonID  string `json:"daemon_id"`
	BotUID    string `json:"bot_uid"`
	ErrorMsg  string `json:"error_msg,omitempty"`
	CreatedAt string `json:"created_at"`
}

type ackBotTaskReq struct {
	ClaimToken    string `json:"claim_token"`
	Status        string `json:"status"` // "succeeded" or "failed"
	ResultSummary string `json:"result_summary,omitempty"`
	ErrorMsg      string `json:"error_msg,omitempty"`
}

func toBotTaskResp(m *botTaskModel) botTaskResp {
	return botTaskResp{
		ID:        m.Id,
		Status:    m.Status,
		AgentID:   m.AgentID,
		DaemonID:  m.DaemonID,
		BotUID:    m.BotUID,
		ErrorMsg:  m.ErrorMsg,
		CreatedAt: formatTime(time.Time(m.CreatedAt)),
	}
}

// ---------- db helpers ----------

func (d *runtimeDB) insertBotTask(m *botTaskModel) (int64, error) {
	res, err := d.session.InsertBySql(
		`INSERT INTO bot_task
		   (matter_id, matter_base_url, space_id, bot_uid, agent_id, daemon_id, runtime_id,
		    requester_uid, title, description, prompt, status, error_msg, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'poc')`,
		m.MatterID, m.MatterBaseURL, m.SpaceID, m.BotUID, m.AgentID, m.DaemonID, m.RuntimeID,
		m.RequesterUID, m.Title, m.Description, m.Prompt, m.Status, m.ErrorMsg,
	).Exec()
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *runtimeDB) queryBotTaskByID(id int64) (*botTaskModel, error) {
	var m botTaskModel
	count, err := d.session.SelectBySql(
		"SELECT "+botTaskSelectColumns+" FROM bot_task WHERE id=?", id,
	).Load(&m)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return &m, nil
}

func (d *runtimeDB) updateBotTaskStatus(id int64, status, resultSummary, errMsg string) error {
	_, err := d.session.UpdateBySql(
		`UPDATE bot_task SET status=?, result_summary=?, error_msg=? WHERE id=?`,
		status, resultSummary, errMsg, id,
	).Exec()
	return err
}

// resolveBotBinding looks up the bot row by uid. Returns workspace_id
// (was agent_id in PoC1 schema; kept as named return for caller compat),
// daemon_id, runtime_id, and runtime_kind. runtime_kind lets the caller
// short-circuit non-openclaw dispatches before queueing a bot_task that
// the daemon would never claim.
func (d *runtimeDB) resolveBotBinding(spaceID, botUID string) (workspaceID, daemonID, runtimeKind string, runtimeID int64, err error) {
	b, qerr := d.resolveBotByUID(spaceID, botUID)
	if qerr != nil {
		return "", "", "", 0, qerr
	}
	if b == nil {
		return "", "", "", 0, fmt.Errorf("no bot has bot_uid=%s in space=%s", botUID, spaceID)
	}
	return b.WorkspaceID, b.DaemonID, b.RuntimeKind, b.RuntimeID, nil
}

// claimPendingBotTask is the heartbeat side of the pull-based dispatch.
// Mirrors claimPendingManagedAgentCommand but for bot_task rows.
func (d *runtimeDB) claimPendingBotTask(daemonID string) (*botTaskModel, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	var m botTaskModel
	count, err := tx.SelectBySql(
		"SELECT "+botTaskSelectColumns+` FROM bot_task
		 WHERE daemon_id=? AND status=?
		 ORDER BY id ASC LIMIT 1 FOR UPDATE`,
		daemonID, btStatusQueued,
	).Load(&m)
	if err != nil || count == 0 {
		return nil, err
	}

	token := randomToken()
	if _, err := tx.UpdateBySql(
		`UPDATE bot_task SET status=?, claim_token=? WHERE id=?`,
		btStatusDispatched, token, m.Id,
	).Exec(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.Status = btStatusDispatched
	m.ClaimToken = token
	return &m, nil
}

// ---------- HTTP handlers ----------

// internalTokenAuth is the same shared-secret scheme used by modules/notify.
// We don't reuse the notify middleware to keep runtime independent of notify;
// both read NOTIFY_INTERNAL_TOKEN from env at process start. If the env is
// unset the middleware fails closed (rejects all requests).
func (rt *Runtime) internalTokenAuth() wkhttp.HandlerFunc {
	token := os.Getenv("NOTIFY_INTERNAL_TOKEN")
	if token == "" {
		rt.Warn("NOTIFY_INTERNAL_TOKEN not set — /v1/internal/bot-tasks will reject all requests")
	}
	return func(c *wkhttp.Context) {
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "internal API auth not configured"})
			return
		}
		hdr := c.GetHeader("X-Internal-Token")
		if subtle.ConstantTimeCompare([]byte(hdr), []byte(token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "unauthorized"})
			return
		}
		c.Next()
	}
}

// POST /v1/internal/bot-tasks
// auth = X-Internal-Token shared secret (called by octo-matter)
//
// Resolves bot_uid → openclaw agent+daemon binding, queues a row,
// and returns immediately. Daemon picks it up via the next heartbeat.
func (rt *Runtime) createBotTask(c *wkhttp.Context) {
	var req createBotTaskReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if strings.TrimSpace(req.MatterID) == "" {
		c.ResponseError(errors.New("matter_id is required"))
		return
	}
	if strings.TrimSpace(req.SpaceID) == "" {
		c.ResponseError(errors.New("space_id is required"))
		return
	}
	if strings.TrimSpace(req.BotUID) == "" {
		c.ResponseError(errors.New("bot_uid is required"))
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		c.ResponseError(errors.New("title is required"))
		return
	}

	row := &botTaskModel{
		MatterID:      req.MatterID,
		MatterBaseURL: strings.TrimRight(req.MatterBaseURL, "/"),
		SpaceID:       req.SpaceID,
		BotUID:        req.BotUID,
		RequesterUID:  req.RequesterUID,
		Title:         req.Title,
		Description:   req.Description,
		Status:        btStatusQueued,
	}
	// Prompt override: caller (matter @mention dispatch) supplies a full
	// prompt that includes conversation history; fall back to the default
	// title+description composition.
	if strings.TrimSpace(req.Prompt) != "" {
		row.Prompt = req.Prompt
	} else {
		row.Prompt = composeBotTaskPrompt(row)
	}

	agentID, daemonID, runtimeKind, runtimeID, err := rt.db.resolveBotBinding(req.SpaceID, req.BotUID)
	if err != nil {
		// Still insert the row so the requester has an id to reference, but
		// mark it failed up front. Saves a round-trip and gives the matter
		// service a stable id to attach the failure comment to.
		row.Status = btStatusFailed
		row.ErrorMsg = fmt.Sprintf("resolve bot binding: %v", err)
		id, ierr := rt.db.insertBotTask(row)
		if ierr != nil {
			rt.Error("insert failed bot_task", zap.Error(ierr))
			c.ResponseError(errors.New("create failed"))
			return
		}
		row.Id = id
		row.CreatedAt = db.Time(time.Now())
		row.UpdatedAt = row.CreatedAt
		// Best-effort write the failure back to the matter timeline so the
		// requester sees something concrete instead of silence.
		go func() {
			if werr := rt.postMatterTimeline(row); werr != nil {
				rt.Warn("matter timeline writeback (resolve-failure) failed",
					zap.Int64("bot_task_id", row.Id),
					zap.Error(werr))
			}
		}()
		c.Response(toBotTaskResp(row))
		return
	}

	// PoC4: non-openclaw runtime — bot exists but daemon won't run anything.
	// Fail-fast with a clear activity message instead of queuing forever.
	if runtimeKind != runtimeKindOpenclaw {
		row.Status = btStatusFailed
		row.ErrorMsg = fmt.Sprintf("runtime %q not supported yet (only openclaw runs tasks in this build)", runtimeKind)
		row.AgentID = agentID
		row.DaemonID = daemonID
		row.RuntimeID = runtimeID
		id, ierr := rt.db.insertBotTask(row)
		if ierr != nil {
			rt.Error("insert inert bot_task", zap.Error(ierr))
			c.ResponseError(errors.New("create failed"))
			return
		}
		row.Id = id
		row.CreatedAt = db.Time(time.Now())
		row.UpdatedAt = row.CreatedAt
		go func() {
			if werr := rt.postMatterTimeline(row); werr != nil {
				rt.Warn("matter timeline writeback (inert) failed", zap.Int64("bot_task_id", row.Id), zap.Error(werr))
			}
			if werr := rt.postMatterActivity(row); werr != nil {
				rt.Warn("matter activity writeback (inert) failed", zap.Int64("bot_task_id", row.Id), zap.Error(werr))
			}
		}()
		c.Response(toBotTaskResp(row))
		return
	}

	row.AgentID = agentID
	row.DaemonID = daemonID
	row.RuntimeID = runtimeID

	id, err := rt.db.insertBotTask(row)
	if err != nil {
		rt.Error("insert bot_task", zap.Error(err))
		c.ResponseError(errors.New("create failed"))
		return
	}
	row.Id = id
	row.CreatedAt = db.Time(time.Now())
	row.UpdatedAt = row.CreatedAt
	rt.Info("bot_task queued", zap.Int64("id", id), zap.String("matter_id", req.MatterID),
		zap.String("bot_uid", req.BotUID), zap.String("agent_id", agentID))
	c.Response(toBotTaskResp(row))
}

// POST /v1/daemon/bot-tasks/:id/ack
// auth = daemon API key
// body: { claim_token, status: "succeeded"|"failed", result_summary?, error_msg? }
//
// Single ack endpoint for both success and failure. Server records status +
// summary, then fires the timeline writeback to octo-matter (best-effort).
func (rt *Runtime) ackBotTask(c *wkhttp.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.ResponseError(errors.New("invalid id"))
		return
	}
	var req ackBotTaskReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.Status != btStatusSucceeded && req.Status != btStatusFailed {
		c.ResponseError(errors.New("status must be 'succeeded' or 'failed'"))
		return
	}

	m, err := rt.db.queryBotTaskByID(id)
	if err != nil || m == nil {
		c.ResponseError(errors.New("bot_task not found"))
		return
	}
	ownerUID := c.MustGet("owner_uid").(string)
	spaceID := c.MustGet("space_id").(string)
	if m.SpaceID != spaceID {
		c.ResponseErrorWithStatus(errors.New("no permission"), http.StatusForbidden)
		return
	}
	_ = ownerUID // bot_task is space-scoped; ownership lives on matter, not here
	if m.ClaimToken == "" || m.ClaimToken != req.ClaimToken {
		c.ResponseErrorWithStatus(errors.New("invalid or stale claim_token"), http.StatusConflict)
		return
	}

	if err := rt.db.updateBotTaskStatus(id, req.Status, req.ResultSummary, req.ErrorMsg); err != nil {
		rt.Error("ack bot_task update", zap.Error(err))
		c.ResponseError(errors.New("ack failed"))
		return
	}
	m.Status = req.Status
	m.ResultSummary = req.ResultSummary
	m.ErrorMsg = req.ErrorMsg

	// Best-effort writeback: log on failure but do not fail the ack. Daemon
	// has already done its job; we don't want it to retry just because matter
	// is briefly down.
	go func() {
		if err := rt.postMatterTimeline(m); err != nil {
			rt.Warn("matter timeline writeback failed",
				zap.Int64("bot_task_id", m.Id),
				zap.String("matter_id", m.MatterID),
				zap.Error(err))
		}
		if err := rt.postMatterActivity(m); err != nil {
			rt.Warn("matter activity writeback failed",
				zap.Int64("bot_task_id", m.Id),
				zap.String("matter_id", m.MatterID),
				zap.Error(err))
		}
	}()

	c.Response(gin.H{"status": "ok"})
}

// buildPendingBotTask renders the heartbeat payload the daemon needs to run
// the task. Kept symmetric with buildPendingAgentCommand for daemon parsing.
func (rt *Runtime) buildPendingBotTask(m *botTaskModel) gin.H {
	return gin.H{
		"id":          m.Id,
		"agent_id":    m.AgentID,
		"prompt":      m.Prompt,
		"matter_id":   m.MatterID,
		"bot_uid":     m.BotUID,
		"claim_token": m.ClaimToken,
	}
}

// composeBotTaskPrompt builds the prompt sent to `openclaw agent -m ...`.
// PoC: just title + description; bot has full openclaw context from its own
// channels.octo binding (it can read the matter via API if needed).
func composeBotTaskPrompt(m *botTaskModel) string {
	var b strings.Builder
	b.WriteString(m.Title)
	if strings.TrimSpace(m.Description) != "" {
		b.WriteString("\n\n")
		b.WriteString(m.Description)
	}
	return b.String()
}

// postMatterTimeline writes the bot's response (or failure message) back to the matter timeline as a comment authored by the bot. Calls octo-matter's
// internal endpoint with the shared X-Internal-Token; octo-matter trusts
// internal callers to specify any actor_uid (bots are users in IM).
//
// PoC: best-effort, no retries. ack handler logs but does not surface failure
// because the daemon already did its job — leaving matter timeline empty is
// a UI annoyance, not a correctness issue.
func (rt *Runtime) postMatterTimeline(m *botTaskModel) error {
	if strings.TrimSpace(m.MatterBaseURL) == "" {
		return fmt.Errorf("matter_base_url empty — nothing to write back to")
	}
	token := os.Getenv("NOTIFY_INTERNAL_TOKEN")
	if token == "" {
		return fmt.Errorf("NOTIFY_INTERNAL_TOKEN unset on server")
	}

	var content string
	switch m.Status {
	case btStatusSucceeded:
		content = strings.TrimSpace(m.ResultSummary)
		if content == "" {
			content = "(agent returned empty response)"
		}
	case btStatusFailed:
		content = fmt.Sprintf("⚠️ agent task failed: %s", strings.TrimSpace(m.ErrorMsg))
	default:
		return fmt.Errorf("unexpected status %s for writeback", m.Status)
	}

	body := map[string]any{
		"actor_uid": m.BotUID,
		"space_id":  m.SpaceID,
		"content":   content,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	endpoint := strings.TrimRight(m.MatterBaseURL, "/") + "/api/v1/internal/matters/" + m.MatterID + "/timeline"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s: status %d body=%s", endpoint, resp.StatusCode, string(respBody))
	}
	return nil
}

// postMatterActivity records an agent_task_completed / agent_task_failed
// activity on the matter so it shows up in the matter's "dynamics" feed —
// timeline writeback is the bot's CONTENT, this one is the structured
// EVENT (icon + label + JSON payload) the feed renders specially. Mirrors
// postMatterTimeline's best-effort + shared X-Internal-Token contract.
func (rt *Runtime) postMatterActivity(m *botTaskModel) error {
	if strings.TrimSpace(m.MatterBaseURL) == "" {
		return fmt.Errorf("matter_base_url empty — nothing to write back to")
	}
	token := os.Getenv("NOTIFY_INTERNAL_TOKEN")
	if token == "" {
		return fmt.Errorf("NOTIFY_INTERNAL_TOKEN unset on server")
	}

	var action string
	detail := map[string]any{
		"bot_uid":  m.BotUID,
		"task_id":  m.Id,
		"agent_id": m.AgentID,
	}
	// elapsed: rough — we use created_at as the start (queued moment). Daemon
	// claim → ack is usually a few seconds after that; for a PoC display this
	// is close enough. Cutover should store started_at when dispatched.
	if !time.Time(m.CreatedAt).IsZero() {
		detail["elapsed_ms"] = time.Since(time.Time(m.CreatedAt)).Milliseconds()
	}
	switch m.Status {
	case btStatusSucceeded:
		action = "agent_task_completed"
		detail["bytes"] = len(m.ResultSummary)
	case btStatusFailed:
		action = "agent_task_failed"
		detail["error"] = strings.TrimSpace(m.ErrorMsg)
	default:
		return fmt.Errorf("unexpected status %s for activity writeback", m.Status)
	}

	body := map[string]any{
		"actor_uid": m.BotUID,
		"action":    action,
		"detail":    detail,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	endpoint := strings.TrimRight(m.MatterBaseURL, "/") + "/api/v1/internal/matters/" + m.MatterID + "/activities"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s: status %d body=%s", endpoint, resp.StatusCode, string(respBody))
	}
	return nil
}
