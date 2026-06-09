package opanalytics

import (
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

// maxPageSize 管理端分页上限，防止超大页把全表拉出。
const maxPageSize = 200

// maxPageIndex 页码上限：防止超大 page_index 让 (pageIndex-1)*pageSize 溢出成负 offset，
// 进而在内存分页(spaceList)切片越界 panic / 在 SQL 路径传负 OFFSET。封顶后越界页返回空列表。
const maxPageIndex = 1_000_000

// maxRangeDays 时间范围上限(含两端约 1 年)。
const maxRangeDays = 366

// Manager 运营分析看板(管理端 superAdmin 跨 space 只读)。
type Manager struct {
	ctx *config.Context
	log.Log
	service   *service
	etl       *ETL
	scheduler *Scheduler
}

// New 创建看板 Manager。生产环境(非测试)自启动每日 ETL 调度器。
func New(ctx *config.Context) *Manager {
	etl := NewETL(ctx)
	m := &Manager{
		ctx:       ctx,
		Log:       log.NewTLog("OpanalyticsManager"),
		service:   newService(ctx),
		etl:       etl,
		scheduler: NewScheduler(ctx, etl),
	}
	if !ctx.GetConfig().Test {
		if err := m.scheduler.Start(); err != nil {
			m.Error("failed to start opanalytics scheduler", zap.Error(err))
		}
	}
	return m
}

// Route 配置路由。统一前缀 /v1/manager/dashboard；逐 handler 校验 superAdmin。
// SharedUIDRateLimiter 挂在 AuthMiddleware 之后(须先解析出 uid)：这些是跨 Space 聚合的重查询，
// 防管理端误刷/脚本轮询/token 泄漏把 DB 打满(每登录用户共享桶，见 pkg/wkhttp/ratelimit_helper.go)。
func (m *Manager) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1/manager/dashboard", m.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, m.ctx))
	{
		auth.GET("/overview", m.overview)                       // 模块A 概览卡片
		auth.GET("/trend", m.trend)                             // 模块C 趋势
		auth.GET("/spaces", m.spaces)                           // 表一 Space 列表
		auth.GET("/spaces/:space_id/channels", m.spaceChannels) // 表二 群组列表(仅群组)
		auth.GET("/global/direct-chats", m.globalDirectChats)   // 全局私聊活跃列表
		auth.POST("/etl/run", m.runETL)                         // 手动触发增量 ETL
	}
}

// overview 模块A 概览(私聊只出活跃数)。
func (m *Manager) overview(c *wkhttp.Context) {
	if !m.requireSuperAdmin(c) {
		return
	}
	start, end, ok := parseDateRange(c)
	if !ok {
		respRequestInvalid(c, "date_range")
		return
	}
	resp, err := m.service.overview(start, end, parseSpaceIDs(c))
	if err != nil {
		m.Error("overview query failed", zap.Error(err))
		respQueryFailed(c)
		return
	}
	c.Response(resp)
}

// trend 模块C 趋势(day/week，缺桶补 0)。
func (m *Manager) trend(c *wkhttp.Context) {
	if !m.requireSuperAdmin(c) {
		return
	}
	start, end, ok := parseDateRange(c)
	if !ok {
		respRequestInvalid(c, "date_range")
		return
	}
	granularity, ok := normalizeGranularity(c.Query("granularity"))
	if !ok {
		respRequestInvalid(c, "granularity")
		return
	}
	resp, err := m.service.trend(start, end, granularity, parseSpaceIDs(c))
	if err != nil {
		m.Error("trend query failed", zap.Error(err))
		respQueryFailed(c)
		return
	}
	c.Response(resp)
}

// spaces 表一 Space 列表。
func (m *Manager) spaces(c *wkhttp.Context) {
	if !m.requireSuperAdmin(c) {
		return
	}
	start, end, ok := parseDateRange(c)
	if !ok {
		respRequestInvalid(c, "date_range")
		return
	}
	pageIndex, pageSize := clampPage(c.GetPage())
	list, total, err := m.service.spaceList(
		start, end, c.Query("name"), normalizeActiveStatus(c.Query("active_status")),
		c.Query("sort_by"), c.Query("order"), (pageIndex-1)*pageSize, pageSize)
	if err != nil {
		m.Error("space list query failed", zap.Error(err))
		respQueryFailed(c)
		return
	}
	c.Response(map[string]interface{}{"count": total, "list": list})
}

// spaceChannels 表二 群组列表(仅群组，私聊不进表二)。
func (m *Manager) spaceChannels(c *wkhttp.Context) {
	if !m.requireSuperAdmin(c) {
		return
	}
	spaceID := c.Param("space_id")
	if spaceID == "" {
		respRequestInvalid(c, "space_id")
		return
	}
	exists, err := m.service.spaceExists(spaceID)
	if err != nil {
		m.Error("space exists check failed", zap.Error(err))
		respQueryFailed(c)
		return
	}
	if !exists {
		respNotFound(c)
		return
	}
	start, end, ok := parseDateRange(c)
	if !ok {
		respRequestInvalid(c, "date_range")
		return
	}
	pageIndex, pageSize := clampPage(c.GetPage())
	list, total, err := m.service.channelList(
		spaceID, start, end, normalizeActiveStatus(c.Query("active_status")),
		c.Query("sort_by"), c.Query("order"), (pageIndex-1)*pageSize, pageSize)
	if err != nil {
		m.Error("channel list query failed", zap.Error(err))
		respQueryFailed(c)
		return
	}
	c.Response(map[string]interface{}{"count": total, "list": list})
}

// globalDirectChats 全局私聊活跃列表(无活跃状态筛选；私聊恒活跃集)。
func (m *Manager) globalDirectChats(c *wkhttp.Context) {
	if !m.requireSuperAdmin(c) {
		return
	}
	start, end, ok := parseDateRange(c)
	if !ok {
		respRequestInvalid(c, "date_range")
		return
	}
	pageIndex, pageSize := clampPage(c.GetPage())
	list, total, err := m.service.directChatList(
		start, end, c.Query("sort_by"), c.Query("order"), (pageIndex-1)*pageSize, pageSize)
	if err != nil {
		m.Error("direct chat list query failed", zap.Error(err))
		respQueryFailed(c)
		return
	}
	c.Response(map[string]interface{}{"count": total, "list": list})
}

// runETL 手动触发一轮增量 ETL。首轮可能是长时间全历史回填，因此只接受任务并异步执行。
func (m *Manager) runETL(c *wkhttp.Context) {
	if !m.requireSuperAdmin(c) {
		return
	}

	accepted, err := m.scheduler.TriggerManualRun()
	if err != nil {
		m.Error("manual opanalytics ETL trigger failed",
			zap.String("uid", c.GetLoginUID()),
			zap.Error(err))
		respETLTriggerFailed(c)
		return
	}
	if !accepted {
		respETLAlreadyRunning(c)
		return
	}

	m.Info("manual opanalytics ETL accepted", zap.String("uid", c.GetLoginUID()))
	c.ResponseWithStatus(http.StatusAccepted, map[string]interface{}{
		"status": http.StatusAccepted,
		"state":  "accepted",
	})
}

// ===== 参数解析 =====

func (m *Manager) requireSuperAdmin(c *wkhttp.Context) bool {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		respForbidden(c)
		return false
	}
	return true
}

// parseDateRange 解析 start_date/end_date(报告时区 YYYY-MM-DD)，默认近 30 天，上限 1 年。
func parseDateRange(c *wkhttp.Context) (string, string, bool) {
	loc := reportLocation()
	const layout = "2006-01-02"

	var end time.Time
	var err error
	if v := c.Query("end_date"); v != "" {
		if end, err = time.ParseInLocation(layout, v, loc); err != nil {
			return "", "", false
		}
	} else {
		now := time.Now().In(loc)
		end = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	}

	var start time.Time
	if v := c.Query("start_date"); v != "" {
		if start, err = time.ParseInLocation(layout, v, loc); err != nil {
			return "", "", false
		}
	} else {
		start = end.AddDate(0, 0, -29)
	}

	if start.After(end) {
		return "", "", false
	}
	// BETWEEN 闭区间：跨度 N 天 = N+1 个自然日。用 >= 使含两端的自然日数封顶为 maxRangeDays。
	if end.Sub(start) >= maxRangeDays*24*time.Hour {
		return "", "", false
	}
	return start.Format(layout), end.Format(layout), true
}

// parseSpaceIDs 读取 space_ids / space_ids[](去空去重)。
func parseSpaceIDs(c *wkhttp.Context) []string {
	raw := append(c.QueryArray("space_ids"), c.QueryArray("space_ids[]")...)
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// normalizeActiveStatus 收敛为 all/active/inactive。
func normalizeActiveStatus(v string) string {
	switch v {
	case "active", "inactive":
		return v
	default:
		return "all"
	}
}

// normalizeGranularity 收敛为 day/week，缺省 day。
func normalizeGranularity(v string) (string, bool) {
	switch v {
	case "", "day":
		return "day", true
	case "week":
		return "week", true
	default:
		return "", false
	}
}

// clampPage 规范化页码/页大小并执行上下限保护(入参直接适配 c.GetPage())。
// 同时封顶 pageIndex，避免 (pageIndex-1)*pageSize 溢出成负 offset 导致切片 panic / 负 OFFSET。
func clampPage(pageIndex, pageSize int64) (int, int) {
	if pageIndex <= 0 {
		pageIndex = 1
	}
	if pageIndex > maxPageIndex {
		pageIndex = maxPageIndex
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	return int(pageIndex), int(pageSize)
}
