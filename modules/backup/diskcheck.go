package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// DiskChecker 磁盘空间检查器
type DiskChecker struct{}

// NewDiskChecker 创建磁盘检查器
func NewDiskChecker() *DiskChecker {
	return &DiskChecker{}
}

// CheckAvailableSpace 检查目录可用空间是否满足需求
// path: 要检查的目录路径
// requiredBytes: 需要的字节数
// 返回: 可用空间(bytes), 是否满足需求, error
func (d *DiskChecker) CheckAvailableSpace(path string, requiredBytes int64) (available int64, sufficient bool, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, false, fmt.Errorf("failed to get disk stats: %w", err)
	}

	// 可用空间 = 可用块数 * 块大小
	available = int64(stat.Bavail) * int64(stat.Bsize)
	sufficient = available >= requiredBytes

	return available, sufficient, nil
}

// EstimateArchiveSize 估算压缩包大小
// sourcePath: 源目录路径
// compressionRatio: 压缩比 (0.0-1.0, 例如 0.5 表示压缩后为原大小的 50%)
func (d *DiskChecker) EstimateArchiveSize(sourcePath string, compressionRatio float64) (int64, error) {
	var totalSize int64

	err := filepath.Walk(sourcePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})

	if err != nil {
		return 0, fmt.Errorf("failed to calculate directory size: %w", err)
	}

	// 应用压缩比
	estimatedSize := int64(float64(totalSize) * compressionRatio)

	// 至少需要 1MB 的安全余量
	const safetyMargin = 1024 * 1024
	return estimatedSize + safetyMargin, nil
}

// CheckBeforeBackup 备份前检查磁盘空间
// sourcePath: 源数据目录
// tempPath: 临时文件目录 (通常是 os.TempDir())
// 返回: error 如果空间不足
func (d *DiskChecker) CheckBeforeBackup(sourcePath, tempPath string) error {
	// 估算压缩包大小 (假设 gzip 压缩比为 30%)
	estimatedSize, err := d.EstimateArchiveSize(sourcePath, 0.3)
	if err != nil {
		return err
	}

	// 检查临时目录空间
	available, sufficient, err := d.CheckAvailableSpace(tempPath, estimatedSize)
	if err != nil {
		return err
	}

	if !sufficient {
		return fmt.Errorf("insufficient disk space: need %s, available %s",
			FormatFileSize(estimatedSize),
			FormatFileSize(available))
	}

	return nil
}
