package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type Runtime struct {
	ctx *config.Context
	log.Log
	db runtimeDB
}

func New(ctx *config.Context) *Runtime {
	rt := &Runtime{
		ctx: ctx,
		Log: log.NewTLog("Runtime"),
		db:  *newRuntimeDB(ctx),
	}
	go rt.runSweeper()

	// 版本同步：从 COS 拉取 version.json 写入 runtime_latest_version。
	// 独立 goroutine，不塞进 runSweeper，失败不影响其他扫描。
	cfgFile := ctx.GetConfig().ConfigFileUsed()
	syncer := newVersionSyncer(&rt.db, cfgFile)
	go syncer.run(context.Background())

	return rt
}

func (rt *Runtime) Route(r *wkhttp.WKHttp) {
	daemon := r.Group("/v1/daemon", rt.authAPIKey())
	{
		daemon.POST("/register", rt.register)
		daemon.POST("/heartbeat", rt.heartbeat)
		daemon.POST("/deregister", rt.deregister)
		daemon.POST("/ping/:ping_id", rt.pingReport)
		daemon.POST("/upgrade/:task_id", rt.upgradeReport)
		daemon.POST("/bots/:id/ack", rt.ackBot)
		daemon.POST("/bot-tasks/:id/ack", rt.ackBotTask)
	}

	internal := r.Group("/v1/internal", rt.internalTokenAuth())
	{
		internal.POST("/bot-tasks", rt.createBotTask)
	}

	auth := r.Group("/v1", rt.ctx.AuthMiddleware(r))
	{
		auth.GET("/runtimes", rt.list)
		auth.DELETE("/runtimes/:id", rt.deleteRuntime)
		auth.POST("/runtimes/ping", rt.pingInit)
		auth.GET("/runtimes/ping/:ping_id", rt.pingGet)
		auth.POST("/runtimes/upgrade", rt.upgradeInit)
		auth.GET("/runtimes/upgrade/:task_id", rt.upgradeGet)
		auth.POST("/runtimes/bots", rt.createBot)
		auth.GET("/runtimes/bots", rt.listBots)
		auth.GET("/runtimes/bots/:id", rt.getBot)
		auth.DELETE("/runtimes/bots/:id", rt.archiveBot)
		auth.GET("/runtimes/bots/:id/feed", rt.getBotFeed)
	}
}

type apiKeyInfo struct {
	UID     string
	SpaceID string
}

func (rt *Runtime) authAPIKey() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "missing Authorization header"})
			return
		}
		apiKey := strings.TrimPrefix(auth, "Bearer ")
		if apiKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "empty api key"})
			return
		}

		var info apiKeyInfo
		_, err := rt.db.session.Select("uid", "space_id").From("user_api_key").
			Where("api_key=?", apiKey).
			Load(&info)
		if err != nil {
			rt.Error("query user_api_key failed", zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "auth failed"})
			return
		}
		if info.UID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "invalid api key"})
			return
		}

		var memberCount int
		if err := rt.db.session.SelectBySql(
			"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
			info.SpaceID, info.UID,
		).LoadOne(&memberCount); err != nil {
			rt.Error("check space membership in authAPIKey failed", zap.Error(err))
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"msg": "auth check failed"})
			return
		}
		if memberCount == 0 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "user is no longer a member of this space"})
			return
		}

		c.Set("owner_uid", info.UID)
		c.Set("space_id", info.SpaceID)
		c.Next()
	}
}

// POST /v1/daemon/register
func (rt *Runtime) register(c *wkhttp.Context) {
	var req registerReq
	if err := c.BindJSON(&req); err != nil {
		rt.Error("bind register request", zap.Error(err))
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.DaemonID == "" {
		c.ResponseError(errors.New("daemon_id is required"))
		return
	}

	ownerUID := c.MustGet("owner_uid").(string)
	spaceID := c.MustGet("space_id").(string)

	var registered []registeredRuntimeResp

	for _, r := range req.Runtimes {
		if r.Type == "" {
			continue
		}
		status := r.Status
		if status == "" {
			status = "online"
		}

		metaMap := map[string]interface{}{
			"cli_version": req.CLIVersion,
		}
		if len(r.Agents) > 0 {
			metaMap["agents"] = r.Agents
		}
		if len(r.Plugins) > 0 {
			metaMap["plugins"] = r.Plugins
		}
		metaBytes, _ := json.Marshal(metaMap)

		m := &agentRuntimeModel{
			SpaceID:     spaceID,
			DaemonID:    req.DaemonID,
			Name:        r.Name,
			Provider:    r.Type,
			RuntimeMode: "local",
			Status:      status,
			Version:     r.Version,
			DeviceName:  req.DeviceName,
			DeviceInfo:  req.DeviceInfo,
			Metadata:    string(metaBytes),
			OwnerUID:    ownerUID,
		}

		id, err := rt.db.upsert(m)
		if err != nil {
			rt.Error("upsert runtime failed", zap.Error(err), zap.String("provider", r.Type))
			c.ResponseError(errors.New("register failed"))
			return
		}

		registered = append(registered, registeredRuntimeResp{
			ID:       id,
			Provider: r.Type,
		})

		// 插件升级关单：用本次 upsert 返回的 id + 插件版本，匹配任务 metadata.runtime_id
		for _, p := range r.Plugins {
			if p.Name != "" && p.Version != "" {
				rt.db.completeUpgradeIfMatchedWithRuntime(req.DaemonID, p.Name, p.Version, id)
			}
		}

		// Provider 组件升级关单（claude/codex/openclaw/hermes）：
		// 按 daemon_id + provider + version + runtime_id 匹配。runtime.version 即 detected CLI 版本。
		// 服务端 createComponentUpgradeTask 只允许 providerComponents 白名单创建任务，
		// 这里无需再次白名单过滤：非白名单 provider 根本不会有对应 in-progress 任务可以关。
		if r.Version != "" {
			rt.db.completeUpgradeIfMatchedWithRuntime(req.DaemonID, r.Type, r.Version, id)
		}
	}

	rt.Info("daemon registered",
		zap.String("daemon_id", req.DaemonID),
		zap.String("owner", ownerUID),
		zap.Int("runtime_count", len(registered)),
	)

	// 升级关单：注册成功后检查是否有匹配的升级任务
	if req.CLIVersion != "" {
		rt.db.completeUpgradeIfMatched(req.DaemonID, "octo-daemon", req.CLIVersion)
	}

	c.Response(gin.H{
		"runtimes": registered,
	})
}

// POST /v1/daemon/heartbeat
func (rt *Runtime) heartbeat(c *wkhttp.Context) {
	var req heartbeatReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.RuntimeID <= 0 {
		c.ResponseError(errors.New("runtime_id is required"))
		return
	}

	ownerUID := c.MustGet("owner_uid").(string)
	spaceID := c.MustGet("space_id").(string)

	existing, err := rt.db.queryByID(req.RuntimeID)
	if err != nil || existing == nil {
		c.ResponseError(errors.New("runtime not found"))
		return
	}
	if existing.OwnerUID != ownerUID || existing.SpaceID != spaceID {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}

	if err := rt.db.updateHeartbeat(req.RuntimeID); err != nil {
		rt.Error("update heartbeat", zap.Error(err), zap.Int64("runtime_id", req.RuntimeID))
		c.ResponseError(errors.New("heartbeat failed"))
		return
	}

	// Atomically claim a pending ping for this daemon (prevents duplicate dispatch)
	resp := gin.H{"status": "ok"}
	claimedPing, _ := rt.db.claimPendingPing(spaceID, existing.DaemonID, time.Now().UnixMilli())
	if claimedPing != nil {
		resp["pending_ping"] = gin.H{
			"ping_id": claimedPing.ID,
		}
	}

	// Atomically claim a pending upgrade task
	claimedUpgrade, _ := rt.db.claimPendingUpgrade(spaceID, existing.DaemonID)
	if claimedUpgrade != nil {
		resp["pending_upgrade"] = gin.H{
			"task_id":        claimedUpgrade.ID,
			"component":      claimedUpgrade.Component,
			"download_url":   claimedUpgrade.DownloadURL,
			"target_version": claimedUpgrade.ToVersion,
			"checksum":       claimedUpgrade.Checksum,
			"metadata":       claimedUpgrade.Metadata,
		}
	}

	// Atomically claim a pending bot.provision command for this daemon.
	// PoC4: single composite command replaces PoC1's two-step agent.create
	// + bot.add cycle.
	claimedBot, _ := rt.db.claimPendingBotProvision(existing.DaemonID)
	if claimedBot != nil {
		resp["pending_command"] = rt.buildPendingBotProvision(claimedBot)
	}

	// Same pull pattern for matter-driven bot tasks.
	claimedTask, _ := rt.db.claimPendingBotTask(existing.DaemonID)
	if claimedTask != nil {
		resp["pending_task"] = rt.buildPendingBotTask(claimedTask)
	}

	c.Response(resp)
}

// POST /v1/daemon/deregister
func (rt *Runtime) deregister(c *wkhttp.Context) {
	var req deregisterReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}

	ownerUID := c.MustGet("owner_uid").(string)
	spaceID := c.MustGet("space_id").(string)

	for _, id := range req.RuntimeIDs {
		existing, err := rt.db.queryByID(id)
		if err != nil {
			rt.Error("query runtime for deregister", zap.Error(err), zap.Int64("id", id))
			c.ResponseError(errors.New("query failed"))
			return
		}
		if existing == nil {
			continue
		}
		if existing.OwnerUID != ownerUID || existing.SpaceID != spaceID {
			c.ResponseErrorWithStatus(errors.New("no permission"), 403)
			return
		}
	}

	if err := rt.db.setOffline(req.RuntimeIDs); err != nil {
		rt.Error("deregister runtimes", zap.Error(err))
		c.ResponseError(errors.New("deregister failed"))
		return
	}

	rt.Info("daemon deregistered", zap.Int("count", len(req.RuntimeIDs)))
	c.ResponseOK()
}

// GET /v1/runtimes?space_id=xxx
func (rt *Runtime) list(c *wkhttp.Context) {
	spaceID := c.Query("space_id")
	if spaceID == "" {
		c.ResponseError(errors.New("space_id is required"))
		return
	}

	loginUID := c.GetLoginUID()
	var memberCount int
	if err := rt.db.session.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		spaceID, loginUID,
	).LoadOne(&memberCount); err != nil {
		rt.Error("check space membership failed", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
		return
	}
	if memberCount == 0 {
		c.ResponseErrorWithStatus(errors.New("no permission to access this space"), 403)
		return
	}

	models, err := rt.db.listBySpaceIDAndOwner(spaceID, loginUID)
	if err != nil {
		rt.Error("list runtimes", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
		return
	}

	list := make([]runtimeResp, 0, len(models))
	for _, m := range models {
		list = append(list, toRuntimeResp(m))
	}

	// 为 OpenClaw runtime 的 agent.routes 注入 route_infos（bot 名字 + 在线态）。
	// 必须在 versionHints 计算之前做，确保所有读 r.Metadata 的后续逻辑看到同一份。
	rt.enrichRuntimeRouteInfos(list, spaceID)

	latestVersions, err := rt.db.queryLatestVersions()
	if err != nil {
		rt.Warn("query latest versions failed (table may not exist)", zap.Error(err))
	}

	// Build version hints per runtime_id
	versionHints := make(map[int64]gin.H)
	if latestVersions != nil {
		for _, r := range list {
			hint := gin.H{}
			hasHint := false

			if latest, ok := latestVersions[r.Provider]; ok && latest != "" && r.Version != "" {
				if isVersionOlder(r.Version, latest) {
					hint["has_update"] = true
					hint["latest_version"] = latest
					hasHint = true
				}
			}

			if r.Provider == "openclaw" && r.Metadata != "" {
				if pluginLatest, ok := latestVersions["octo"]; ok && pluginLatest != "" {
					var meta map[string]interface{}
					if json.Unmarshal([]byte(r.Metadata), &meta) == nil {
						plugins, _ := meta["plugins"].([]interface{})
						for _, p := range plugins {
							pm, _ := p.(map[string]interface{})
							if pm["name"] == "octo" {
								pv, _ := pm["version"].(string)
								if pv != "" && isVersionOlder(pv, pluginLatest) {
									hint["plugin_has_update"] = true
									hint["plugin_latest_version"] = pluginLatest
									hasHint = true
								}
							}
						}
					}
				}
			}

			if hasHint {
				versionHints[r.ID] = hint
			}
		}
	}

	// Build daemon version hints per daemon_id
	daemonVersionHints := make(map[string]gin.H)
	if latestVersions != nil {
		if daemonLatest, ok := latestVersions["octo-daemon"]; ok && daemonLatest != "" {
			seen := make(map[string]bool)
			for _, r := range list {
				if seen[r.DaemonID] {
					continue
				}
				seen[r.DaemonID] = true
				var meta map[string]interface{}
				if r.Metadata != "" {
					json.Unmarshal([]byte(r.Metadata), &meta)
				}
				cliVer, _ := meta["cli_version"].(string)
				if cliVer != "" && isVersionOlder(cliVer, daemonLatest) {
					daemonVersionHints[r.DaemonID] = gin.H{
						"has_update":     true,
						"latest_version": daemonLatest,
						"current":        cliVer,
					}
				}
			}
		}
	}

	// 查询每个 (daemon_id, component) 最新的进行中升级任务
	// 改成数组响应，供前端按 runtime_id / daemon_id + component 恢复按钮态。
	// failed/timeout 是终态，不占 active slot —— 用户应当能立刻重新点 Upgrade
	// 重建新任务；只把真正 in-progress 的状态列进来。
	activeUpgrades := make([]activeUpgradeItem, 0)
	var upgradeTasks []upgradeTask
	rt.db.session.SelectBySql(
		`SELECT t.id, t.daemon_id, t.component, t.status, t.from_version, t.to_version, COALESCE(t.metadata,'') as metadata, COALESCE(t.error_msg,'') as error_msg
		 FROM runtime_upgrade_task t
		 INNER JOIN (
		   SELECT daemon_id, component, MAX(created_at) as max_created
		   FROM runtime_upgrade_task
		   WHERE space_id=? AND owner_uid=?
		   AND status IN ('pending','dispatched','downloading','installing','restarting')
		   GROUP BY daemon_id, component
		 ) latest ON t.daemon_id = latest.daemon_id AND t.component = latest.component AND t.created_at = latest.max_created
		 WHERE t.space_id=? AND t.owner_uid=?`,
		spaceID, loginUID, spaceID, loginUID,
	).Load(&upgradeTasks)
	for _, t := range upgradeTasks {
		item := activeUpgradeItem{
			TaskID:      t.ID,
			DaemonID:    t.DaemonID,
			Component:   t.Component,
			Status:      t.Status,
			FromVersion: t.FromVersion,
			ToVersion:   t.ToVersion,
			ErrorMsg:    t.ErrorMsg,
		}
		// 插件任务 metadata 里的 runtime_id 透出
		if t.Metadata != "" {
			var m struct {
				RuntimeID int64 `json:"runtime_id"`
			}
			if json.Unmarshal([]byte(t.Metadata), &m) == nil {
				item.RuntimeID = m.RuntimeID
			}
		}
		activeUpgrades = append(activeUpgrades, item)
	}

	c.Response(gin.H{
		"runtimes":             list,
		"version_hints":        versionHints,
		"daemon_version_hints": daemonVersionHints,
		"active_upgrades":      activeUpgrades,
	})
}

// DELETE /v1/runtimes/:id
func (rt *Runtime) deleteRuntime(c *wkhttp.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.ResponseError(errors.New("invalid runtime id"))
		return
	}

	existing, err := rt.db.queryByID(id)
	if err != nil {
		rt.Error("query runtime for delete", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
		return
	}
	if existing == nil {
		c.ResponseError(errors.New("runtime not found"))
		return
	}

	loginUID := c.GetLoginUID()
	if existing.OwnerUID != loginUID {
		c.ResponseErrorWithStatus(errors.New("no permission to delete this runtime"), 403)
		return
	}

	if err := rt.db.deleteByID(id); err != nil {
		rt.Error("delete runtime", zap.Error(err))
		c.ResponseError(errors.New("delete failed"))
		return
	}

	rt.Info("runtime deleted", zap.Int64("id", id), zap.String("provider", existing.Provider))
	c.ResponseOK()
}

// POST /v1/runtimes/ping — initiate ping to a daemon
func (rt *Runtime) pingInit(c *wkhttp.Context) {
	var req pingInitReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.DaemonID == "" || req.SpaceID == "" {
		c.ResponseError(errors.New("daemon_id and space_id are required"))
		return
	}

	loginUID := c.GetLoginUID()
	var accessCount int
	if err := rt.db.session.SelectBySql(
		`SELECT COUNT(*) FROM agent_runtime ar
		 INNER JOIN space_member sm ON ar.space_id = sm.space_id COLLATE utf8mb4_general_ci AND sm.uid = ? AND sm.status = 1
		 WHERE ar.space_id = ? AND ar.daemon_id = ?`,
		loginUID, req.SpaceID, req.DaemonID,
	).LoadOne(&accessCount); err != nil {
		rt.Error("check daemon access", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
		return
	}
	if accessCount == 0 {
		c.ResponseErrorWithStatus(errors.New("no permission to ping this device"), 403)
		return
	}

	pingID := fmt.Sprintf("ping_%d", time.Now().UnixNano())
	entry := &pingEntry{
		ID:       pingID,
		DaemonID: req.DaemonID,
		SpaceID:  req.SpaceID,
		ServerTS: time.Now().UnixMilli(),
		Status:   "pending",
	}
	if err := rt.db.insertPing(entry); err != nil {
		rt.Error("insert ping", zap.Error(err))
		c.ResponseError(errors.New("ping init failed"))
		return
	}

	go func() {
		time.Sleep(30 * time.Second)
		rt.db.timeoutPing(pingID)
	}()

	c.Response(gin.H{"ping_id": pingID})
}

// POST /v1/daemon/ping/:ping_id — daemon reports ping result
func (rt *Runtime) pingReport(c *wkhttp.Context) {
	pingID := c.Param("ping_id")

	entry, err := rt.db.getPing(pingID)
	if err != nil || entry == nil {
		c.ResponseError(errors.New("ping not found"))
		return
	}

	ownerUID := c.MustGet("owner_uid").(string)
	apiSpaceID := c.MustGet("space_id").(string)
	if entry.SpaceID != apiSpaceID {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}
	var daemonMatch int
	_ = rt.db.session.SelectBySql(
		`SELECT COUNT(*) FROM agent_runtime WHERE space_id=? AND daemon_id=? AND owner_uid=?`,
		entry.SpaceID, entry.DaemonID, ownerUID,
	).LoadOne(&daemonMatch)
	if daemonMatch == 0 {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}

	// RTT = now (server receives report) - server_ts (server dispatched via heartbeat)
	nowMS := time.Now().UnixMilli()
	rtt := nowMS - entry.ServerTS
	if rtt < 0 {
		rtt = 0
	}

	if err := rt.db.updatePingResult(pingID, nowMS, rtt); err != nil {
		rt.Error("update ping result", zap.Error(err))
		c.ResponseError(errors.New("update failed"))
		return
	}

	c.Response(gin.H{"status": "ok"})
}

// GET /v1/runtimes/ping/:ping_id — get ping result
func (rt *Runtime) pingGet(c *wkhttp.Context) {
	pingID := c.Param("ping_id")
	entry, err := rt.db.getPing(pingID)
	if err != nil || entry == nil {
		c.ResponseError(errors.New("ping not found"))
		return
	}

	loginUID := c.GetLoginUID()
	var memberCount int
	_ = rt.db.session.SelectBySql(
		`SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1`,
		entry.SpaceID, loginUID,
	).LoadOne(&memberCount)
	if memberCount == 0 {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}

	c.Response(gin.H{
		"ping_id": entry.ID,
		"status":  entry.Status,
		"rtt_ms":  entry.RTT,
	})
}

func isVersionOlder(current, latest string) bool {
	// "dev", "unknown", empty → always older than any real version
	if current == "dev" || current == "unknown" || current == "" {
		return latest != "" && latest != "dev" && latest != "unknown"
	}

	parse := func(v string) []int {
		v = strings.TrimPrefix(v, "v")
		for _, sep := range []string{"-", "+"} {
			if idx := strings.Index(v, sep); idx > 0 {
				v = v[:idx]
			}
		}
		parts := strings.Split(v, ".")
		nums := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil
			}
			nums = append(nums, n)
		}
		return nums
	}

	c := parse(current)
	l := parse(latest)
	if c == nil || l == nil {
		return false
	}

	maxLen := len(c)
	if len(l) > maxLen {
		maxLen = len(l)
	}
	for i := 0; i < maxLen; i++ {
		cv, lv := 0, 0
		if i < len(c) {
			cv = c[i]
		}
		if i < len(l) {
			lv = l[i]
		}
		if cv < lv {
			return true
		}
		if cv > lv {
			return false
		}
	}
	return false
}

func (rt *Runtime) runSweeper() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		staleThreshold := 45 * time.Second
		n, err := rt.db.markStaleOffline(staleThreshold)
		if err != nil {
			rt.Error("sweep stale runtimes", zap.Error(err))
			continue
		}
		if n > 0 {
			rt.Info("marked stale runtimes offline", zap.Int64("count", n))
		}

		gcThreshold := 7 * 24 * time.Hour
		deleted, err := rt.db.deleteStaleOffline(gcThreshold)
		if err != nil {
			rt.Error("gc offline runtimes", zap.Error(err))
			continue
		}
		if deleted > 0 {
			rt.Info("gc'd old offline runtimes", zap.Int64("count", deleted))
		}

		if cleaned, err := rt.db.cleanOldPings(5 * time.Minute); err != nil {
			rt.Error("clean old pings", zap.Error(err))
		} else if cleaned > 0 {
			rt.Info("cleaned old ping entries", zap.Int64("count", cleaned))
		}

		rt.db.timeoutStaleUpgrades()
	}
}

func toRuntimeResp(m *agentRuntimeModel) runtimeResp {
	return runtimeResp{
		ID:          m.Id,
		SpaceID:     m.SpaceID,
		DaemonID:    m.DaemonID,
		Name:        m.Name,
		Provider:    m.Provider,
		RuntimeMode: m.RuntimeMode,
		Status:      m.Status,
		Version:     m.Version,
		DeviceName:  m.DeviceName,
		DeviceInfo:  m.DeviceInfo,
		Metadata:    m.Metadata,
		OwnerUID:    m.OwnerUID,
		LastSeenAt:  formatTime(time.Time(m.LastSeenAt)),
		CreatedAt:   formatTime(time.Time(m.CreatedAt)),
		UpdatedAt:   formatTime(time.Time(m.UpdatedAt)),
	}
}
