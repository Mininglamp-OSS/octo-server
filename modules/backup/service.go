package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service 备份服务
type Service struct {
	log.Log
	ctx       *config.Context
	db        *backupDB
	storage   IStorage
	cfg       *BackupConfig
	isRunning bool
	mu        sync.Mutex
}

// NewService 创建备份服务
func NewService(ctx *config.Context, db *backupDB) *Service {
	return &Service{
		Log: log.NewTLog("BackupService"),
		ctx: ctx,
		db:  db,
	}
}

// GetConfig 获取备份配置
func (s *Service) GetConfig() (*BackupConfig, error) {
	return s.db.GetConfig()
}

// SaveConfig 保存备份配置
func (s *Service) SaveConfig(cfg *BackupConfig) error {
	return s.db.SaveConfig(cfg)
}

// TestConnection 测试存储连接（使用系统 COS 配置）
func (s *Service) TestConnection() error {
	cos := s.ctx.GetConfig().COS
	if cos.Bucket == "" {
		return fmt.Errorf("COS Bucket 未配置")
	}
	if cos.SecretID == "" || cos.SecretKey == "" {
		return fmt.Errorf("COS AccessKey/SecretKey 未配置")
	}
	if cos.Region == "" {
		return fmt.Errorf("COS Region 未配置")
	}

	storage, err := NewCOSStorage(&StorageConfig{
		Bucket:    cos.Bucket,
		AccessKey: cos.SecretID,
		SecretKey: cos.SecretKey,
		Region:    cos.Region,
	})
	if err != nil {
		return err
	}

	// 1 分钟超时
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return storage.TestConnection(ctx)
}

// TriggerBackup 手动触发备份
func (s *Service) TriggerBackup() (string, error) {
	s.mu.Lock()
	if s.isRunning {
		s.mu.Unlock()
		return "", fmt.Errorf("backup is already running")
	}
	s.isRunning = true
	s.mu.Unlock()

	backupID := uuid.New().String()

	// 异步执行备份
	go func() {
		defer func() {
			s.mu.Lock()
			s.isRunning = false
			s.mu.Unlock()
		}()

		if err := s.ExecuteBackup(backupID); err != nil {
			s.Error("backup failed", zap.String("backupID", backupID), zap.Error(err))
		}
	}()

	return backupID, nil
}

// ExecuteBackup 执行备份
func (s *Service) ExecuteBackup(backupID string) error {
	s.Info("starting backup", zap.String("backupID", backupID))

	// 创建带超时的 context（30 分钟）
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// 1. 获取配置
	cfg, err := s.db.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get config: %w", err)
	}
	if cfg == nil {
		return fmt.Errorf("backup config not found")
	}

	// 2. 创建备份记录
	if err := s.db.CreateHistory(backupID, BackupStatusRunning); err != nil {
		return fmt.Errorf("failed to create history: %w", err)
	}

	// 3. 检查数据目录
	dataDir := cfg.DataDir
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		s.db.UpdateHistoryStatus(backupID, BackupStatusFailed, fmt.Sprintf("data directory not found: %s", dataDir))
		return fmt.Errorf("data directory not found: %s", dataDir)
	}

	// 4. 检查磁盘空间
	tmpDir := os.TempDir()
	diskChecker := NewDiskChecker()
	if err := diskChecker.CheckBeforeBackup(dataDir, tmpDir); err != nil {
		s.db.UpdateHistoryStatus(backupID, BackupStatusFailed, fmt.Sprintf("disk space check failed: %v", err))
		return fmt.Errorf("disk space check failed: %w", err)
	}

	// 5. 创建临时文件
	fileName := fmt.Sprintf("wukongim-%s.tar.gz", time.Now().Format("20060102-150405"))
	localPath := filepath.Join(tmpDir, fileName)

	// 6. 打包数据目录
	s.Info("creating tar.gz archive", zap.String("source", dataDir), zap.String("dest", localPath))
	if err := s.createTarGz(dataDir, localPath); err != nil {
		s.db.UpdateHistoryStatus(backupID, BackupStatusFailed, fmt.Sprintf("failed to create archive: %v", err))
		return fmt.Errorf("failed to create archive: %w", err)
	}
	defer os.Remove(localPath)

	// 7. 获取文件大小
	fileInfo, err := os.Stat(localPath)
	if err != nil {
		s.db.UpdateHistoryStatus(backupID, BackupStatusFailed, fmt.Sprintf("failed to stat archive: %v", err))
		return fmt.Errorf("failed to stat archive: %w", err)
	}
	fileSize := fileInfo.Size()
	s.Info("archive created", zap.String("file", fileName), zap.Int64("size", fileSize))

	// 8. 创建存储实例（复用 ctx.GetConfig().COS 配置）
	storage, err := s.createStorage(cfg.Prefix)
	if err != nil {
		s.db.UpdateHistoryStatus(backupID, BackupStatusFailed, fmt.Sprintf("failed to create storage: %v", err))
		return fmt.Errorf("failed to create storage: %w", err)
	}

	// 9. 上传到存储
	s.Info("uploading archive", zap.String("file", fileName))
	if err := storage.Upload(ctx, localPath, fileName); err != nil {
		s.db.UpdateHistoryStatus(backupID, BackupStatusFailed, fmt.Sprintf("failed to upload: %v", err))
		return fmt.Errorf("failed to upload: %w", err)
	}

	// 10. 更新备份记录
	remotePath := path.Join(cfg.Prefix, fileName)
	if err := s.db.UpdateHistorySuccess(backupID, fileName, remotePath, fileSize); err != nil {
		return fmt.Errorf("failed to update history: %w", err)
	}

	// 11. 清理旧备份
	if err := s.cleanupOldBackups(ctx, storage, cfg.RetentionCount); err != nil {
		s.Warn("failed to cleanup old backups", zap.Error(err))
	}

	s.Info("backup completed successfully", zap.String("backupID", backupID), zap.String("file", fileName))
	return nil
}

// createTarGz 创建 tar.gz 压缩包
func (s *Service) createTarGz(source, target string) error {
	tarFile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer tarFile.Close()

	gzWriter := gzip.NewWriter(tarFile)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	baseDir := filepath.Base(source)

	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// 创建 tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}

		// 设置相对路径
		relPath, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			header.Name = baseDir
		} else {
			header.Name = filepath.Join(baseDir, relPath)
		}

		// 写入 header
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		// 如果是文件，写入内容
		if !info.IsDir() {
			if err := s.writeFileToTar(tarWriter, path); err != nil {
				return err
			}
		}

		return nil
	})
}

// writeFileToTar 写入单个文件到 tar（确保文件句柄及时关闭）
func (s *Service) writeFileToTar(tarWriter *tar.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(tarWriter, file)
	return err
}

// cleanupOldBackups 清理旧备份
func (s *Service) cleanupOldBackups(ctx context.Context, storage IStorage, keepCount int) error {
	// 获取需要删除的旧备份
	oldBackups, err := s.db.GetOldestHistories(keepCount)
	if err != nil {
		return err
	}

	for _, backup := range oldBackups {
		s.Info("deleting old backup", zap.String("backupID", backup.BackupID), zap.String("file", backup.FileName))

		// 从存储删除
		if err := storage.Delete(ctx, backup.FileName); err != nil {
			s.Warn("failed to delete backup from storage", zap.String("file", backup.FileName), zap.Error(err))
		}

		// 从数据库删除记录
		if err := s.db.DeleteHistory(backup.ID); err != nil {
			s.Warn("failed to delete backup history", zap.Int64("id", backup.ID), zap.Error(err))
		}
	}

	return nil
}

// GetHistoryList 获取备份历史列表
func (s *Service) GetHistoryList(pageIndex, pageSize int) ([]*BackupHistoryResp, int64, error) {
	histories, err := s.db.GetHistoryList(pageIndex, pageSize)
	if err != nil {
		return nil, 0, err
	}

	count, err := s.db.GetHistoryCount()
	if err != nil {
		return nil, 0, err
	}

	result := make([]*BackupHistoryResp, len(histories))
	for i, h := range histories {
		result[i] = &BackupHistoryResp{
			ID:          h.ID,
			BackupID:    h.BackupID,
			Status:      h.Status,
			FileName:    h.FileName,
			FileSize:    h.FileSize,
			FileSizeStr: FormatFileSize(h.FileSize),
			StoragePath: h.StoragePath,
			CreatedAt:   h.CreatedAt.Format("2006-01-02 15:04:05"),
		}
		if h.StartedAt != nil {
			result[i].StartedAt = h.StartedAt.Format("2006-01-02 15:04:05")
		}
		if h.FinishedAt != nil {
			result[i].FinishedAt = h.FinishedAt.Format("2006-01-02 15:04:05")
		}
		if h.ErrorMessage != "" {
			result[i].ErrorMessage = h.ErrorMessage
		}
	}

	return result, count, nil
}

// DeleteHistory 删除备份历史
func (s *Service) DeleteHistory(id int64) error {
	history, err := s.db.GetHistoryByID(id)
	if err != nil {
		return err
	}
	if history == nil {
		return fmt.Errorf("backup history not found")
	}

	// 获取配置以创建存储实例
	cfg, err := s.db.GetConfig()
	if err == nil && cfg != nil {
		storage, err := s.createStorage(cfg.Prefix)
		if err == nil {
			// 尝试从存储删除文件（5 分钟超时）
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := storage.Delete(ctx, history.FileName); err != nil {
				s.Warn("failed to delete backup from storage", zap.String("file", history.FileName), zap.Error(err))
			}
		}
	}

	return s.db.DeleteHistory(id)
}

// GetDownloadURL 获取下载链接
func (s *Service) GetDownloadURL(id int64) (string, error) {
	history, err := s.db.GetHistoryByID(id)
	if err != nil {
		return "", err
	}
	if history == nil {
		return "", fmt.Errorf("backup history not found")
	}

	cfg, err := s.db.GetConfig()
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "", fmt.Errorf("backup config not found")
	}

	storage, err := s.createStorage(cfg.Prefix)
	if err != nil {
		return "", err
	}

	// 生成 1 小时有效的下载链接（1 分钟超时）
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return storage.GetPresignedURL(ctx, history.FileName, time.Hour)
}

// IsRunning 检查是否正在运行备份
func (s *Service) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isRunning
}

// createStorage 创建存储实例（复用 ctx.GetConfig().COS 配置）
func (s *Service) createStorage(backupPrefix string) (IStorage, error) {
	cos := s.ctx.GetConfig().COS
	if cos.Bucket == "" {
		return nil, fmt.Errorf("COS Bucket 未配置")
	}
	if cos.SecretID == "" || cos.SecretKey == "" {
		return nil, fmt.Errorf("COS AccessKey/SecretKey 未配置")
	}
	if cos.Region == "" {
		return nil, fmt.Errorf("COS Region 未配置")
	}

	return NewCOSStorage(&StorageConfig{
		Bucket:    cos.Bucket,
		AccessKey: cos.SecretID,
		SecretKey: cos.SecretKey,
		Region:    cos.Region,
		Prefix:    backupPrefix,
	})
}
