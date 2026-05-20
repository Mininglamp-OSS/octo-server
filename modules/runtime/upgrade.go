package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	componentDaemon = "octo-daemon"
	componentPlugin = "octo"

	daemonUpgradeTimeoutSec = 120  // 2 分钟
	pluginUpgradeTimeoutSec = 600  // 10 分钟（npm install + 依赖 + gateway restart）
)

// providerComponents 是允许远程升级的 provider 组件白名单（对应 agent_runtime.provider）。
// 每个都是 1 daemon × 1 runtime 的关系；升级命令由各自 CLI 自带的 update 子命令负责。
// 服务端 timeout 统一比 daemon 侧 exec timeout 略长，避免 daemon 还没来得及上报 failed 就 timeout。
var providerComponents = map[string]int{
	"claude":   600,  // 10 min
	"codex":    600,  // 10 min
	"openclaw": 720,  // 12 min（npm install + gateway restart）
	"hermes":   1200, // 20 min（git pull + pip reinstall）
}

func isProviderComponent(c string) bool {
	_, ok := providerComponents[c]
	return ok
}

// POST /v1/runtimes/upgrade
func (rt *Runtime) upgradeInit(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	var req upgradeInitReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.DaemonID == "" || req.SpaceID == "" {
		c.ResponseError(errors.New("daemon_id and space_id are required"))
		return
	}
	component := req.Component
	if component == "" {
		component = componentDaemon
	}

	// 1. 校验 space 成员
	var memberCount int
	if err := rt.db.session.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		req.SpaceID, loginUID,
	).LoadOne(&memberCount); err != nil || memberCount == 0 {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}

	// 2. 查 daemon
	var daemon agentRuntimeModel
	_, err := rt.db.session.Select("*").From("agent_runtime").
		Where("space_id=? AND daemon_id=? AND owner_uid=?", req.SpaceID, req.DaemonID, loginUID).
		Limit(1).Load(&daemon)
	if err != nil || daemon.DaemonID == "" {
		c.ResponseErrorWithStatus(errors.New("daemon not found or not owned by you"), 403)
		return
	}

	// 3. 检查 daemon 至少一个 runtime 在线
	var onlineCount int
	rt.db.session.SelectBySql(
		"SELECT COUNT(*) FROM agent_runtime WHERE space_id=? AND daemon_id=? AND owner_uid=? AND status='online'",
		req.SpaceID, req.DaemonID, loginUID,
	).LoadOne(&onlineCount)
	if onlineCount == 0 {
		c.ResponseError(errors.New("daemon is offline, cannot upgrade"))
		return
	}

	// 按 component 分流校验 + 收集任务字段
	switch {
	case component == componentDaemon:
		rt.createDaemonUpgradeTask(c, loginUID, &req, &daemon)
	case component == componentPlugin:
		rt.createPluginUpgradeTask(c, loginUID, &req, &daemon)
	case isProviderComponent(component):
		rt.createComponentUpgradeTask(c, loginUID, &req, &daemon, component)
	default:
		c.ResponseError(fmt.Errorf("unsupported component: %s", component))
	}
}

// createComponentUpgradeTask 处理 provider 组件（claude/codex/hermes/openclaw）升级。
// 校验：runtime_id 归属当前用户、runtime.Provider == component、当前版本严格落后于 latest。
func (rt *Runtime) createComponentUpgradeTask(c *wkhttp.Context, loginUID string, req *upgradeInitReq, daemon *agentRuntimeModel, component string) {
	if req.RuntimeID == 0 {
		c.ResponseError(errors.New("runtime_id is required for component upgrade"))
		return
	}

	// 查 runtime 并强校验：provider 必须等于 component
	var runtime agentRuntimeModel
	_, err := rt.db.session.Select("*").From("agent_runtime").
		Where("id=? AND space_id=? AND daemon_id=? AND owner_uid=?",
			req.RuntimeID, req.SpaceID, req.DaemonID, loginUID).
		Limit(1).Load(&runtime)
	if err != nil || runtime.Id == 0 {
		c.ResponseErrorWithStatus(errors.New("runtime not found or not owned by you"), 403)
		return
	}
	if runtime.Provider != component {
		c.ResponseError(fmt.Errorf("component %s does not match runtime provider %s", component, runtime.Provider))
		return
	}

	// 版本对比：runtime.Version 是 daemon 上报的当前版本
	fromVersion := runtime.Version
	if fromVersion == "" {
		c.ResponseError(errors.New("current version not available on runtime"))
		return
	}

	var versionRow struct {
		LatestVersion string `db:"latest_version"`
	}
	_, err = rt.db.session.SelectBySql(
		"SELECT latest_version FROM runtime_latest_version WHERE component=?",
		component,
	).Load(&versionRow)
	if err != nil || versionRow.LatestVersion == "" {
		c.ResponseError(fmt.Errorf("no latest version available for %s", component))
		return
	}

	// 严格落后检查：dev/unknown 视为比任何正式版本都旧
	if fromVersion == versionRow.LatestVersion {
		c.ResponseError(errors.New("already up to date"))
		return
	}
	if fromVersion != "dev" && fromVersion != "unknown" {
		if !isVersionOlder(fromVersion, versionRow.LatestVersion) {
			c.ResponseError(errors.New("downgrade not allowed"))
			return
		}
	}

	// runtime_id 放到 task.metadata，供 completeUpgradeIfMatchedWithRuntime 关单时对齐
	taskMeta, _ := json.Marshal(map[string]interface{}{
		"runtime_id": req.RuntimeID,
	})

	rt.insertUpgradeTask(c, insertTaskArgs{
		SpaceID:     req.SpaceID,
		DaemonID:    req.DaemonID,
		OwnerUID:    loginUID,
		Component:   component,
		FromVersion: fromVersion,
		ToVersion:   versionRow.LatestVersion,
		DownloadURL: "",
		Checksum:    "",
		Metadata:    string(taskMeta),
	})
}

// octo-daemon 升级：现有逻辑
func (rt *Runtime) createDaemonUpgradeTask(c *wkhttp.Context, loginUID string, req *upgradeInitReq, daemon *agentRuntimeModel) {
	// OS 检查
	var deviceInfo struct {
		OS   string `json:"os"`
		Arch string `json:"arch"`
	}
	json.Unmarshal([]byte(daemon.DeviceInfo), &deviceInfo)
	if deviceInfo.OS == "windows" {
		c.ResponseError(errors.New("Windows remote upgrade is not supported in v1"))
		return
	}

	// 查最新版本 + release_meta
	var versionRow struct {
		LatestVersion string `db:"latest_version"`
		ReleaseMeta   string `db:"release_meta"`
	}
	_, err := rt.db.session.SelectBySql(
		"SELECT latest_version, COALESCE(release_meta,'') as release_meta FROM runtime_latest_version WHERE component=?",
		componentDaemon,
	).Load(&versionRow)
	if err != nil || versionRow.LatestVersion == "" {
		c.ResponseError(errors.New("no latest version available for octo-daemon"))
		return
	}

	// 当前版本
	var metaJSON struct {
		CLIVersion string `json:"cli_version"`
	}
	json.Unmarshal([]byte(daemon.Metadata), &metaJSON)
	fromVersion := metaJSON.CLIVersion

	if fromVersion != "" && fromVersion == versionRow.LatestVersion {
		c.ResponseError(errors.New("already up to date"))
		return
	}
	if fromVersion != "" && fromVersion != "dev" && fromVersion != "unknown" {
		if !isVersionOlder(fromVersion, versionRow.LatestVersion) {
			c.ResponseError(errors.New("downgrade not allowed"))
			return
		}
	}

	// 匹配 asset
	if versionRow.ReleaseMeta == "" {
		c.ResponseError(errors.New("no release metadata available"))
		return
	}
	var meta releaseMetaJSON
	if err := json.Unmarshal([]byte(versionRow.ReleaseMeta), &meta); err != nil {
		rt.Error("parse release_meta", zap.Error(err))
		c.ResponseError(errors.New("invalid release metadata"))
		return
	}
	osName := normalizeOS(deviceInfo.OS)
	archName := normalizeArch(deviceInfo.Arch)
	var matchedAsset *releaseAssetJSON
	for i, a := range meta.Assets {
		if a.Kind == "archive" && a.OS == osName && a.Arch == archName {
			matchedAsset = &meta.Assets[i]
			break
		}
	}
	if matchedAsset == nil {
		c.ResponseError(fmt.Errorf("no matching asset for %s/%s", osName, archName))
		return
	}
	checksum := meta.Checksums[matchedAsset.Name]
	if checksum == "" {
		c.ResponseError(errors.New("no checksum for asset"))
		return
	}

	rt.insertUpgradeTask(c, insertTaskArgs{
		SpaceID:     req.SpaceID,
		DaemonID:    req.DaemonID,
		OwnerUID:    loginUID,
		Component:   componentDaemon,
		FromVersion: fromVersion,
		ToVersion:   versionRow.LatestVersion,
		DownloadURL: matchedAsset.URL,
		Checksum:    checksum,
		Metadata:    "",
	})
}

// octo 插件升级
func (rt *Runtime) createPluginUpgradeTask(c *wkhttp.Context, loginUID string, req *upgradeInitReq, daemon *agentRuntimeModel) {
	if req.RuntimeID == 0 {
		c.ResponseError(errors.New("runtime_id is required for plugin upgrade"))
		return
	}

	// 查 runtime（provider=openclaw + 归属 loginUID + 同 daemon_id）
	var runtime agentRuntimeModel
	_, err := rt.db.session.Select("*").From("agent_runtime").
		Where("id=? AND space_id=? AND daemon_id=? AND owner_uid=?",
			req.RuntimeID, req.SpaceID, req.DaemonID, loginUID).
		Limit(1).Load(&runtime)
	if err != nil || runtime.Id == 0 {
		c.ResponseErrorWithStatus(errors.New("runtime not found or not owned by you"), 403)
		return
	}
	if runtime.Provider != "openclaw" {
		c.ResponseError(errors.New("plugin upgrade only supports openclaw runtime"))
		return
	}

	// 从 metadata.plugins 里找当前插件版本
	var metaJSON struct {
		Plugins []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	json.Unmarshal([]byte(runtime.Metadata), &metaJSON)
	fromVersion := ""
	for _, p := range metaJSON.Plugins {
		if p.Name == componentPlugin {
			fromVersion = p.Version
			break
		}
	}
	if fromVersion == "" {
		c.ResponseError(fmt.Errorf("%s not installed on this runtime", componentPlugin))
		return
	}

	// 查最新版本
	var versionRow struct {
		LatestVersion string `db:"latest_version"`
	}
	_, err = rt.db.session.SelectBySql(
		"SELECT latest_version FROM runtime_latest_version WHERE component=?",
		componentPlugin,
	).Load(&versionRow)
	if err != nil || versionRow.LatestVersion == "" {
		c.ResponseError(errors.New("no latest version available for " + componentPlugin))
		return
	}

	// 版本校验：必须严格落后
	if fromVersion == versionRow.LatestVersion {
		c.ResponseError(errors.New("already up to date"))
		return
	}
	if !isVersionOlder(fromVersion, versionRow.LatestVersion) {
		c.ResponseError(errors.New("downgrade not allowed"))
		return
	}

	// 任务 metadata 记录 runtime_id（用于关单校验）
	taskMeta, _ := json.Marshal(map[string]interface{}{
		"runtime_id": req.RuntimeID,
	})

	rt.insertUpgradeTask(c, insertTaskArgs{
		SpaceID:     req.SpaceID,
		DaemonID:    req.DaemonID,
		OwnerUID:    loginUID,
		Component:   componentPlugin,
		FromVersion: fromVersion,
		ToVersion:   versionRow.LatestVersion,
		DownloadURL: "",
		Checksum:    "",
		Metadata:    string(taskMeta),
	})
}

type insertTaskArgs struct {
	SpaceID     string
	DaemonID    string
	OwnerUID    string
	Component   string
	FromVersion string
	ToVersion   string
	DownloadURL string
	Checksum    string
	Metadata    string
}

// 互斥：同 daemon_id 只允许一个 in-progress 任务（无论 component）
// 关键：没有现有任务时 SELECT COUNT(*) ... FOR UPDATE 不锁任何行，并发 upgradeInit
// 仍可能都看到 0 各自插入。先锁 agent_runtime 里 daemon_id 对应的某一行强制串行化，
// 所有同 daemon 的并发请求都会卡在同一把锁上。
func (rt *Runtime) insertUpgradeTask(c *wkhttp.Context, args insertTaskArgs) {
	tx, err := rt.db.session.Begin()
	if err != nil {
		c.ResponseError(errors.New("internal error"))
		return
	}
	defer tx.RollbackUnlessCommitted()

	// 先锁 agent_runtime 中该 daemon 的任一行（LIMIT 1），强制同 daemon 并发串行
	var lockRow struct {
		ID int64 `db:"id"`
	}
	_, err = tx.SelectBySql(
		`SELECT id FROM agent_runtime WHERE daemon_id=? AND space_id=? ORDER BY id LIMIT 1 FOR UPDATE`,
		args.DaemonID, args.SpaceID,
	).Load(&lockRow)
	if err != nil {
		c.ResponseError(errors.New("internal error"))
		return
	}

	var activeCount int
	tx.SelectBySql(
		`SELECT COUNT(*) FROM runtime_upgrade_task
		 WHERE daemon_id=?
		 AND status IN ('pending','dispatched','downloading','installing','restarting')`,
		args.DaemonID,
	).LoadOne(&activeCount)
	if activeCount > 0 {
		c.ResponseError(errors.New("an upgrade is already in progress for this daemon"))
		return
	}

	taskID := fmt.Sprintf("upgrade_%d", snowflakeID())
	_, err = tx.InsertBySql(
		`INSERT INTO runtime_upgrade_task (id, space_id, daemon_id, owner_uid, component, from_version, to_version, download_url, checksum, metadata, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
		taskID, args.SpaceID, args.DaemonID, args.OwnerUID, args.Component,
		args.FromVersion, args.ToVersion, args.DownloadURL, args.Checksum, args.Metadata,
	).Exec()
	if err != nil {
		rt.Error("create upgrade task", zap.Error(err))
		c.ResponseError(errors.New("create upgrade task failed"))
		return
	}
	tx.Commit()

	c.Response(gin.H{"task_id": taskID})
}

// GET /v1/runtimes/upgrade/:task_id
func (rt *Runtime) upgradeGet(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	taskID := c.Param("task_id")

	var task upgradeTask
	_, err := rt.db.session.SelectBySql(
		`SELECT id, space_id, daemon_id, owner_uid, component, from_version, to_version, download_url, checksum, COALESCE(metadata,'') as metadata, status, COALESCE(error_msg,'') as error_msg
		 FROM runtime_upgrade_task WHERE id=?`, taskID,
	).Load(&task)
	if err != nil || task.ID == "" {
		c.ResponseError(errors.New("task not found"))
		return
	}

	if task.OwnerUID != loginUID {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}

	c.Response(gin.H{
		"id":           task.ID,
		"component":    task.Component,
		"status":       task.Status,
		"from_version": task.FromVersion,
		"to_version":   task.ToVersion,
		"error_msg":    task.ErrorMsg,
	})
}

// POST /v1/daemon/upgrade/:task_id
func (rt *Runtime) upgradeReport(c *wkhttp.Context) {
	ownerUID := c.MustGet("owner_uid").(string)
	apiSpaceID := c.MustGet("space_id").(string)
	taskID := c.Param("task_id")

	var req upgradeReportReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}

	var task upgradeTask
	_, err := rt.db.session.SelectBySql(
		`SELECT id, space_id, daemon_id, owner_uid, component, from_version, to_version, download_url, checksum, COALESCE(metadata,'') as metadata, status, COALESCE(error_msg,'') as error_msg
		 FROM runtime_upgrade_task WHERE id=?`, taskID,
	).Load(&task)
	if err != nil || task.ID == "" {
		c.ResponseError(errors.New("task not found"))
		return
	}
	if task.SpaceID != apiSpaceID || task.OwnerUID != ownerUID {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}

	// 按 component 放行状态流转
	allowed := validTransitionsFrom(task.Component, req.Status)
	if allowed == nil {
		c.ResponseError(errors.New("invalid status transition"))
		return
	}

	result, err := rt.db.session.UpdateBySql(
		fmt.Sprintf(
			`UPDATE runtime_upgrade_task SET status=?, error_msg=?, updated_at=NOW()
			 WHERE id=? AND status IN (%s)`,
			placeholders(len(allowed)),
		),
		append([]interface{}{req.Status, req.Error, taskID}, toInterfaces(allowed)...)...,
	).Exec()
	if err != nil {
		rt.Error("update upgrade task", zap.Error(err))
		c.ResponseError(errors.New("update failed"))
		return
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		c.ResponseError(errors.New("invalid state transition"))
		return
	}
	c.Response(gin.H{"status": "ok"})
}

// 按 component 返回目标状态允许从哪些前置状态流转而来；nil 表示不允许此状态。
// octo-daemon 有完整 5 态机；其他所有组件（plugin + provider 组件）
// 都走精简的 dispatched → installing → (completed by register / failed) 3 态机。
func validTransitionsFrom(component, target string) []string {
	if component == componentDaemon {
		switch target {
		case "downloading":
			return []string{"dispatched"}
		case "installing":
			return []string{"downloading"}
		case "restarting":
			return []string{"installing"}
		case "failed":
			return []string{"dispatched", "downloading", "installing", "restarting"}
		}
		return nil
	}
	// plugin + provider 组件
	switch target {
	case "installing":
		return []string{"dispatched"}
	case "failed":
		return []string{"dispatched", "installing"}
	}
	return nil
}

// DB operations for upgrade

// claim：去掉 component 过滤，同 daemon_id 下取最新 pending task
func (d *runtimeDB) claimPendingUpgrade(spaceID, daemonID string) (*upgradeTask, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	var task upgradeTask
	_, err = tx.SelectBySql(
		`SELECT id, space_id, daemon_id, owner_uid, component, from_version, to_version, download_url, checksum, COALESCE(metadata,'') as metadata, status
		 FROM runtime_upgrade_task
		 WHERE space_id=? AND daemon_id=? AND status='pending'
		 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
		spaceID, daemonID,
	).Load(&task)
	if err != nil {
		return nil, err
	}
	if task.ID == "" {
		return nil, nil
	}

	_, err = tx.UpdateBySql(
		`UPDATE runtime_upgrade_task SET status='dispatched', updated_at=NOW() WHERE id=?`,
		task.ID,
	).Exec()
	if err != nil {
		return nil, err
	}
	tx.Commit()

	task.Status = "dispatched"
	return &task, nil
}

// daemon 升级关单：按 daemon_id + component + version 三元组
func (d *runtimeDB) completeUpgradeIfMatched(daemonID, component, version string) {
	d.session.UpdateBySql(
		`UPDATE runtime_upgrade_task SET status='completed', updated_at=NOW()
		 WHERE daemon_id=? AND component=? AND to_version=?
		 AND status IN ('dispatched','downloading','installing','restarting')`,
		daemonID, component, version,
	).Exec()
}

// 插件升级关单：候选集按 daemon_id + component + in-progress + runtime_id 过滤；
// 版本对比不要求精确相等（npx 安装的版本可能比任务创建时的 to_version 更新），
// 只要 actual_version >= to_version 就关单。
func (d *runtimeDB) completeUpgradeIfMatchedWithRuntime(daemonID, component, actualVersion string, runtimeID int64) {
	var candidates []upgradeTask
	_, err := d.session.SelectBySql(
		`SELECT id, space_id, daemon_id, owner_uid, component, from_version, to_version, download_url, checksum, COALESCE(metadata,'') as metadata, status
		 FROM runtime_upgrade_task
		 WHERE daemon_id=? AND component=?
		 AND status IN ('dispatched','downloading','installing','restarting')`,
		daemonID, component,
	).Load(&candidates)
	if err != nil {
		return
	}
	for _, t := range candidates {
		// runtime_id 归属校验
		var m struct {
			RuntimeID int64 `json:"runtime_id"`
		}
		if t.Metadata != "" {
			json.Unmarshal([]byte(t.Metadata), &m)
		}
		if m.RuntimeID != runtimeID {
			continue
		}
		// 版本校验：actual >= to_version
		if actualVersion != t.ToVersion && !isVersionOlder(t.ToVersion, actualVersion) {
			continue
		}
		d.session.UpdateBySql(
			`UPDATE runtime_upgrade_task SET status='completed', updated_at=NOW() WHERE id=?`,
			t.ID,
		).Exec()
	}
}

// sweeper：按 component 差异化 timeout
func (d *runtimeDB) timeoutStaleUpgrades() {
	// octo-daemon: 120s，完整 5 态机
	d.session.UpdateBySql(
		`UPDATE runtime_upgrade_task SET status='timeout', error_msg='upgrade timed out', updated_at=NOW()
		 WHERE component=?
		 AND status IN ('pending','dispatched','downloading','installing','restarting')
		 AND updated_at < DATE_SUB(NOW(), INTERVAL ? SECOND)`,
		componentDaemon, daemonUpgradeTimeoutSec,
	).Exec()
	// plugin: 600s，精简 3 态机
	d.session.UpdateBySql(
		`UPDATE runtime_upgrade_task SET status='timeout', error_msg='upgrade timed out', updated_at=NOW()
		 WHERE component=?
		 AND status IN ('pending','dispatched','installing')
		 AND updated_at < DATE_SUB(NOW(), INTERVAL ? SECOND)`,
		componentPlugin, pluginUpgradeTimeoutSec,
	).Exec()
	// provider 组件（claude/codex/openclaw/hermes）：各自 timeout，3 态机
	for component, timeoutSec := range providerComponents {
		d.session.UpdateBySql(
			`UPDATE runtime_upgrade_task SET status='timeout', error_msg='upgrade timed out', updated_at=NOW()
			 WHERE component=?
			 AND status IN ('pending','dispatched','installing')
			 AND updated_at < DATE_SUB(NOW(), INTERVAL ? SECOND)`,
			component, timeoutSec,
		).Exec()
	}
}

// helpers

func normalizeOS(os string) string {
	switch strings.ToLower(os) {
	case "macos":
		return "darwin"
	default:
		return strings.ToLower(os)
	}
}

func normalizeArch(arch string) string {
	switch strings.ToLower(arch) {
	case "x86_64", "x64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return strings.ToLower(arch)
	}
}

func snowflakeID() int64 {
	return time.Now().UnixNano()
}

func placeholders(n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = "?"
	}
	return strings.Join(p, ",")
}

func toInterfaces(ss []string) []interface{} {
	r := make([]interface{}, len(ss))
	for i, s := range ss {
		r[i] = s
	}
	return r
}
