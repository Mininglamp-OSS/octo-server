package backup

import (
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// Manager 备份管理
type Manager struct {
	ctx       *config.Context
	db        *backupDB
	service   *Service
	scheduler *Scheduler
	log.Log
}

// NewManager 创建备份管理
func NewManager(ctx *config.Context) *Manager {
	db := newBackupDB(ctx)
	service := NewService(ctx, db)
	scheduler := NewScheduler(service)

	m := &Manager{
		ctx:       ctx,
		db:        db,
		service:   service,
		scheduler: scheduler,
		Log:       log.NewTLog("BackupManager"),
	}

	// 启动定时调度器
	if err := scheduler.Start(); err != nil {
		m.Error("failed to start backup scheduler", zap.Error(err))
	}

	return m
}

// Route 配置路由规则
func (m *Manager) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1/manager", m.ctx.AuthMiddleware(r))
	{
		// 备份配置
		auth.GET("/backup/config", m.getConfig)
		auth.PUT("/backup/config", m.updateConfig)
		auth.POST("/backup/config/test", m.testConnection)

		// 备份操作
		auth.POST("/backup/trigger", m.triggerBackup)
		auth.GET("/backup/history", m.getHistory)
		auth.DELETE("/backup/history/:id", m.deleteHistory)
		auth.GET("/backup/history/:id/download", m.getDownloadURL)

		// 状态
		auth.GET("/backup/status", m.getStatus)
	}
}

// getConfig 获取备份配置
func (m *Manager) getConfig(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}

	cfg, err := m.service.GetConfig()
	if err != nil {
		m.Error("failed to get backup config", zap.Error(err))
		c.ResponseError(errors.New("获取备份配置失败"))
		return
	}

	// 从系统配置获取 COS 信息（只读展示）
	cos := m.ctx.GetConfig().COS

	if cfg == nil {
		// 返回默认配置
		c.Response(&BackupConfigResp{
			Enabled:        false,
			Prefix:         "backup/",
			CronExpr:       "0 2 * * *",
			RetentionCount: 7,
			DataDir:        "/data/wukongim",
			// 只读的系统 COS 配置
			StorageType: "cos",
			Bucket:      cos.Bucket,
			Region:      cos.Region,
		})
		return
	}

	c.Response(&BackupConfigResp{
		Enabled:        cfg.Enabled,
		Prefix:         cfg.Prefix,
		CronExpr:       cfg.CronExpr,
		RetentionCount: cfg.RetentionCount,
		DataDir:        cfg.DataDir,
		// 只读的系统 COS 配置
		StorageType: "cos",
		Bucket:      cos.Bucket,
		Region:      cos.Region,
	})
}

// updateConfig 更新备份配置
func (m *Manager) updateConfig(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}

	var req BackupConfigReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("参数错误"))
		return
	}

	// 获取现有配置
	existingCfg, err := m.service.GetConfig()
	if err != nil {
		m.Error("failed to get existing config", zap.Error(err))
		c.ResponseError(errors.New("获取配置失败"))
		return
	}

	// 合并配置（存储配置复用系统 COS，这里只处理备份相关配置）
	cfg := &BackupConfig{
		Prefix:   req.Prefix,
		CronExpr: req.CronExpr,
		DataDir:  req.DataDir,
	}

	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	} else if existingCfg != nil {
		cfg.Enabled = existingCfg.Enabled
	}

	if req.RetentionCount != nil {
		cfg.RetentionCount = *req.RetentionCount
	} else if existingCfg != nil {
		cfg.RetentionCount = existingCfg.RetentionCount
	} else {
		cfg.RetentionCount = 7
	}

	// 设置默认值
	if cfg.Prefix == "" {
		cfg.Prefix = "backup/"
	}
	if cfg.CronExpr == "" {
		cfg.CronExpr = "0 2 * * *"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/data/wukongim"
	}

	// 验证 cron 表达式
	if err := ValidateCronExpr(cfg.CronExpr); err != nil {
		c.ResponseError(errors.New("无效的 cron 表达式: " + err.Error()))
		return
	}

	// 保存配置
	if err := m.service.SaveConfig(cfg); err != nil {
		m.Error("failed to save backup config", zap.Error(err))
		c.ResponseError(errors.New("保存配置失败"))
		return
	}

	// 更新定时任务
	if err := m.scheduler.UpdateSchedule(cfg.CronExpr, cfg.Enabled); err != nil {
		m.Warn("failed to update scheduler", zap.Error(err))
	}

	c.ResponseOK()
}

// testConnection 测试存储连接（测试系统 COS 配置）
func (m *Manager) testConnection(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}

	err := m.service.TestConnection()
	if err != nil {
		m.Error("connection test failed", zap.Error(err))
		c.ResponseError(errors.New("连接测试失败: " + err.Error()))
		return
	}

	c.ResponseOK()
}

// triggerBackup 手动触发备份
func (m *Manager) triggerBackup(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}

	backupID, err := m.service.TriggerBackup()
	if err != nil {
		m.Error("failed to trigger backup", zap.Error(err))
		c.ResponseError(errors.New("触发备份失败: " + err.Error()))
		return
	}

	c.Response(map[string]string{
		"backup_id": backupID,
		"message":   "备份已开始，请稍后查看备份历史",
	})
}

// getHistory 获取备份历史
func (m *Manager) getHistory(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}

	pageIndex, _ := strconv.Atoi(c.DefaultQuery("page_index", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	if pageIndex < 1 {
		pageIndex = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	list, count, err := m.service.GetHistoryList(pageIndex, pageSize)
	if err != nil {
		m.Error("failed to get backup history", zap.Error(err))
		c.ResponseError(errors.New("获取备份历史失败"))
		return
	}

	c.Response(map[string]interface{}{
		"list":  list,
		"count": count,
	})
}

// deleteHistory 删除备份历史
func (m *Manager) deleteHistory(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.ResponseError(errors.New("无效的ID"))
		return
	}

	if err := m.service.DeleteHistory(id); err != nil {
		m.Error("failed to delete backup history", zap.Error(err))
		c.ResponseError(errors.New("删除备份失败"))
		return
	}

	c.ResponseOK()
}

// getDownloadURL 获取下载链接
func (m *Manager) getDownloadURL(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.ResponseError(errors.New("无效的ID"))
		return
	}

	url, err := m.service.GetDownloadURL(id)
	if err != nil {
		m.Error("failed to get download URL", zap.Error(err))
		c.ResponseError(errors.New("获取下载链接失败: " + err.Error()))
		return
	}

	c.Response(map[string]string{
		"url": url,
	})
}

// getStatus 获取备份状态
func (m *Manager) getStatus(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}

	c.Response(map[string]interface{}{
		"is_running": m.service.IsRunning(),
		"next_run":   m.scheduler.GetNextRun(),
	})
}
