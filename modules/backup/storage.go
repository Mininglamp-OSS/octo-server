package backup

import (
	"context"
	"fmt"
	"io"
	"time"
)

// IStorage 存储接口
type IStorage interface {
	// Upload 上传文件
	Upload(ctx context.Context, localPath, remotePath string) error
	// Delete 删除文件
	Delete(ctx context.Context, remotePath string) error
	// GetPresignedURL 获取预签名下载URL
	GetPresignedURL(ctx context.Context, remotePath string, expires time.Duration) (string, error)
	// TestConnection 测试连接
	TestConnection(ctx context.Context) error
	// List 列出文件
	List(ctx context.Context, prefix string) ([]string, error)
}

// StorageConfig 存储配置（复用系统 COS 配置）
type StorageConfig struct {
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	Prefix    string // 备份路径前缀
}

// FormatFileSize 格式化文件大小
func FormatFileSize(size int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case size >= GB:
		return fmt.Sprintf("%.2f GB", float64(size)/float64(GB))
	case size >= MB:
		return fmt.Sprintf("%.2f MB", float64(size)/float64(MB))
	case size >= KB:
		return fmt.Sprintf("%.2f KB", float64(size)/float64(KB))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// ProgressReader 进度读取器
type ProgressReader struct {
	reader     io.Reader
	total      int64
	read       int64
	onProgress func(read, total int64)
}

func NewProgressReader(reader io.Reader, total int64, onProgress func(read, total int64)) *ProgressReader {
	return &ProgressReader{
		reader:     reader,
		total:      total,
		onProgress: onProgress,
	}
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.read += int64(n)
	if pr.onProgress != nil {
		pr.onProgress(pr.read, pr.total)
	}
	return n, err
}
