package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDiskChecker_CheckAvailableSpace(t *testing.T) {
	checker := NewDiskChecker()

	// 检查临时目录（应该存在且有空间）
	tmpDir := os.TempDir()
	available, sufficient, err := checker.CheckAvailableSpace(tmpDir, 1024) // 需要 1KB

	assert.NoError(t, err)
	assert.True(t, available > 0, "should have available space")
	assert.True(t, sufficient, "1KB should be sufficient")
}

func TestDiskChecker_CheckAvailableSpace_InsufficientSpace(t *testing.T) {
	checker := NewDiskChecker()

	tmpDir := os.TempDir()
	// 请求一个超大空间 (1 PB)
	_, sufficient, err := checker.CheckAvailableSpace(tmpDir, 1024*1024*1024*1024*1024)

	assert.NoError(t, err)
	assert.False(t, sufficient, "1PB should not be sufficient")
}

func TestDiskChecker_CheckAvailableSpace_InvalidPath(t *testing.T) {
	checker := NewDiskChecker()

	_, _, err := checker.CheckAvailableSpace("/nonexistent/path/12345", 1024)

	assert.Error(t, err)
}

func TestDiskChecker_EstimateArchiveSize(t *testing.T) {
	checker := NewDiskChecker()

	// 创建临时目录和文件
	tmpDir, err := os.MkdirTemp("", "diskcheck_test")
	assert.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// 创建一个 1KB 的文件
	testFile := filepath.Join(tmpDir, "test.txt")
	data := make([]byte, 1024)
	err = os.WriteFile(testFile, data, 0644)
	assert.NoError(t, err)

	// 估算大小 (30% 压缩比)
	estimatedSize, err := checker.EstimateArchiveSize(tmpDir, 0.3)
	assert.NoError(t, err)

	// 估算大小应该是 1024 * 0.3 + 1MB 安全余量 ≈ 1MB+
	assert.True(t, estimatedSize > 1024*1024, "should include safety margin")
	assert.True(t, estimatedSize < 2*1024*1024, "should not be too large")
}

func TestDiskChecker_EstimateArchiveSize_EmptyDir(t *testing.T) {
	checker := NewDiskChecker()

	// 创建空临时目录
	tmpDir, err := os.MkdirTemp("", "diskcheck_empty")
	assert.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	estimatedSize, err := checker.EstimateArchiveSize(tmpDir, 0.3)
	assert.NoError(t, err)

	// 空目录只有安全余量
	assert.Equal(t, int64(1024*1024), estimatedSize)
}

func TestDiskChecker_EstimateArchiveSize_InvalidPath(t *testing.T) {
	checker := NewDiskChecker()

	_, err := checker.EstimateArchiveSize("/nonexistent/path/12345", 0.3)
	assert.Error(t, err)
}

func TestDiskChecker_CheckBeforeBackup(t *testing.T) {
	checker := NewDiskChecker()

	// 创建临时目录和小文件
	tmpDir, err := os.MkdirTemp("", "diskcheck_backup")
	assert.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "test.txt")
	err = os.WriteFile(testFile, []byte("hello"), 0644)
	assert.NoError(t, err)

	// 检查应该通过
	err = checker.CheckBeforeBackup(tmpDir, os.TempDir())
	assert.NoError(t, err)
}

func TestDiskChecker_CheckBeforeBackup_InvalidSourcePath(t *testing.T) {
	checker := NewDiskChecker()

	err := checker.CheckBeforeBackup("/nonexistent/path", os.TempDir())
	assert.Error(t, err)
}
