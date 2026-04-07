package backup

import "time"

// BackupConfig 备份配置模型（存储配置复用 ctx.GetConfig().COS）
type BackupConfig struct {
	ID             int64     `json:"id"`
	Enabled        bool      `json:"enabled"`
	Prefix         string    `json:"prefix"`          // 备份路径前缀
	CronExpr       string    `json:"cron_expr"`       // cron表达式
	RetentionCount int       `json:"retention_count"` // 保留数量
	DataDir        string    `json:"data_dir"`        // WuKongIM 数据目录
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// BackupHistory 备份历史模型
type BackupHistory struct {
	ID           int64      `json:"id"`
	BackupID     string     `json:"backup_id"`
	Status       string     `json:"status"` // pending/running/success/failed
	FileName     string     `json:"file_name"`
	FileSize     int64      `json:"file_size"`
	StoragePath  string     `json:"storage_path"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	ErrorMessage string     `json:"error_message"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// BackupConfigReq 备份配置请求
type BackupConfigReq struct {
	Enabled        *bool  `json:"enabled,omitempty"`
	Prefix         string `json:"prefix,omitempty"`
	CronExpr       string `json:"cron_expr,omitempty"`
	RetentionCount *int   `json:"retention_count,omitempty"`
	DataDir        string `json:"data_dir,omitempty"`
}

// BackupConfigResp 备份配置响应
type BackupConfigResp struct {
	Enabled        bool   `json:"enabled"`
	Prefix         string `json:"prefix"`
	CronExpr       string `json:"cron_expr"`
	RetentionCount int    `json:"retention_count"`
	DataDir        string `json:"data_dir"`
	// 以下字段从 ctx.GetConfig() 读取，只读展示
	StorageType string `json:"storage_type"`
	Bucket      string `json:"bucket"`
	Region      string `json:"region"`
}

// BackupHistoryResp 备份历史响应
type BackupHistoryResp struct {
	ID           int64  `json:"id"`
	BackupID     string `json:"backup_id"`
	Status       string `json:"status"`
	FileName     string `json:"file_name"`
	FileSize     int64  `json:"file_size"`
	FileSizeStr  string `json:"file_size_str"`
	StoragePath  string `json:"storage_path"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// BackupStatus 备份状态常量
const (
	BackupStatusPending = "pending"
	BackupStatusRunning = "running"
	BackupStatusSuccess = "success"
	BackupStatusFailed  = "failed"
)
