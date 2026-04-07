package backup

import (
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

type backupDB struct {
	session *dbr.Session
	ctx     *config.Context
}

func newBackupDB(ctx *config.Context) *backupDB {
	return &backupDB{
		session: ctx.DB(),
		ctx:     ctx,
	}
}

// ==================== BackupConfig ====================

// GetConfig 获取备份配置
func (d *backupDB) GetConfig() (*BackupConfig, error) {
	var m *backupConfigModel
	_, err := d.session.Select("*").From("backup_config").OrderDesc("id").Limit(1).Load(&m)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	return m.toBackupConfig(), nil
}

// SaveConfig 保存备份配置
func (d *backupDB) SaveConfig(cfg *BackupConfig) error {
	existing, err := d.GetConfig()
	if err != nil {
		return err
	}

	m := &backupConfigModel{
		Enabled:        boolToInt(cfg.Enabled),
		Prefix:         cfg.Prefix,
		CronExpr:       cfg.CronExpr,
		RetentionCount: cfg.RetentionCount,
		DataDir:        cfg.DataDir,
	}

	if existing == nil {
		_, err = d.session.InsertInto("backup_config").Columns(
			"enabled", "prefix", "cron_expr", "retention_count", "data_dir",
		).Values(
			m.Enabled, m.Prefix, m.CronExpr, m.RetentionCount, m.DataDir,
		).Exec()
	} else {
		_, err = d.session.Update("backup_config").SetMap(map[string]interface{}{
			"enabled":         m.Enabled,
			"prefix":          m.Prefix,
			"cron_expr":       m.CronExpr,
			"retention_count": m.RetentionCount,
			"data_dir":        m.DataDir,
		}).Where("id=?", existing.ID).Exec()
	}
	return err
}

// ==================== BackupHistory ====================

// CreateHistory 创建备份历史记录
func (d *backupDB) CreateHistory(backupID, status string) error {
	now := time.Now()
	_, err := d.session.InsertInto("backup_history").Columns(
		"backup_id", "status", "started_at",
	).Values(
		backupID, status, now,
	).Exec()
	return err
}

// UpdateHistoryStatus 更新备份状态
func (d *backupDB) UpdateHistoryStatus(backupID, status, errorMsg string) error {
	updateMap := map[string]interface{}{
		"status": status,
	}
	if errorMsg != "" {
		updateMap["error_message"] = errorMsg
	}
	if status == BackupStatusFailed || status == BackupStatusSuccess {
		updateMap["finished_at"] = time.Now()
	}
	_, err := d.session.Update("backup_history").SetMap(updateMap).Where("backup_id=?", backupID).Exec()
	return err
}

// UpdateHistorySuccess 更新备份成功状态
func (d *backupDB) UpdateHistorySuccess(backupID, fileName, storagePath string, fileSize int64) error {
	now := time.Now()
	_, err := d.session.Update("backup_history").SetMap(map[string]interface{}{
		"status":       BackupStatusSuccess,
		"file_name":    fileName,
		"storage_path": storagePath,
		"file_size":    fileSize,
		"finished_at":  now,
	}).Where("backup_id=?", backupID).Exec()
	return err
}

// GetHistoryList 获取备份历史列表
func (d *backupDB) GetHistoryList(pageIndex, pageSize int) ([]*BackupHistory, error) {
	var models []*backupHistoryModel
	_, err := d.session.Select("*").From("backup_history").
		OrderDesc("created_at").
		Offset(uint64((pageIndex - 1) * pageSize)).
		Limit(uint64(pageSize)).
		Load(&models)
	if err != nil {
		return nil, err
	}

	result := make([]*BackupHistory, len(models))
	for i, m := range models {
		result[i] = m.toBackupHistory()
	}
	return result, nil
}

// GetHistoryCount 获取备份历史总数
func (d *backupDB) GetHistoryCount() (int64, error) {
	var count int64
	err := d.session.Select("count(*)").From("backup_history").LoadOne(&count)
	return count, err
}

// GetHistoryByID 根据ID获取备份历史
func (d *backupDB) GetHistoryByID(id int64) (*BackupHistory, error) {
	var m *backupHistoryModel
	_, err := d.session.Select("*").From("backup_history").Where("id=?", id).Load(&m)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	return m.toBackupHistory(), nil
}

// DeleteHistory 删除备份历史
func (d *backupDB) DeleteHistory(id int64) error {
	_, err := d.session.DeleteFrom("backup_history").Where("id=?", id).Exec()
	return err
}

// GetOldestHistories 获取最旧的备份记录（用于清理）
func (d *backupDB) GetOldestHistories(keepCount int) ([]*BackupHistory, error) {
	var models []*backupHistoryModel
	_, err := d.session.Select("*").From("backup_history").
		Where("status=?", BackupStatusSuccess).
		OrderDesc("created_at").
		Offset(uint64(keepCount)).
		Load(&models)
	if err != nil {
		return nil, err
	}

	result := make([]*BackupHistory, len(models))
	for i, m := range models {
		result[i] = m.toBackupHistory()
	}
	return result, nil
}

// ==================== Internal Models ====================

type backupConfigModel struct {
	ID             int64     `db:"id"`
	Enabled        int       `db:"enabled"`
	Prefix         string    `db:"prefix"`
	CronExpr       string    `db:"cron_expr"`
	RetentionCount int       `db:"retention_count"`
	DataDir        string    `db:"data_dir"`
	CreatedAt      time.Time `db:"created_at"`
	UpdatedAt      time.Time `db:"updated_at"`
}

func (m *backupConfigModel) toBackupConfig() *BackupConfig {
	return &BackupConfig{
		ID:             m.ID,
		Enabled:        m.Enabled == 1,
		Prefix:         m.Prefix,
		CronExpr:       m.CronExpr,
		RetentionCount: m.RetentionCount,
		DataDir:        m.DataDir,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
	}
}

type backupHistoryModel struct {
	ID           int64      `db:"id"`
	BackupID     string     `db:"backup_id"`
	Status       string     `db:"status"`
	FileName     string     `db:"file_name"`
	FileSize     int64      `db:"file_size"`
	StoragePath  string     `db:"storage_path"`
	StartedAt    *time.Time `db:"started_at"`
	FinishedAt   *time.Time `db:"finished_at"`
	ErrorMessage *string    `db:"error_message"`
	CreatedAt    time.Time  `db:"created_at"`
	UpdatedAt    time.Time  `db:"updated_at"`
}

func (m *backupHistoryModel) toBackupHistory() *BackupHistory {
	h := &BackupHistory{
		ID:          m.ID,
		BackupID:    m.BackupID,
		Status:      m.Status,
		FileName:    m.FileName,
		FileSize:    m.FileSize,
		StoragePath: m.StoragePath,
		StartedAt:   m.StartedAt,
		FinishedAt:  m.FinishedAt,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
	if m.ErrorMessage != nil {
		h.ErrorMessage = *m.ErrorMessage
	}
	return h
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
