package thread

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// TestMain 把 thread 包跑 DB 集成测试需要的全局前置一次性做掉：
//   - OCTO_MASTER_KEY：common.Setup 强制要求（见 user/main_test.go）
//   - DM_THREAD_ON=true：thread 模块在 1module.go:26 用此 env 当 Beta 开关
//   - robot 表手工建：thread 通过 1module.go imports group，触发 space-20260308-01.sql
//     migration，该 migration JOIN robot 表，但 robot 模块未被 import 时其建表迁移不会跑
//     （已知问题，group/api_test.go:33 同样手工建过）
func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		_ = os.Setenv("OCTO_MASTER_KEY", hex.EncodeToString(key))
	}
	_ = os.Setenv("DM_THREAD_ON", "true")

	db, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=true")
	if err == nil {
		defer db.Close()
		// 直接把唯一索引内联到 CREATE TABLE — MySQL 5.7/8.0 都支持 CREATE TABLE IF NOT EXISTS
		// 内的 UNIQUE KEY，避免单独跑 CREATE UNIQUE INDEX IF NOT EXISTS（部分 MySQL 版本不支持
		// 该语法）。
		_, _ = db.Exec("CREATE TABLE IF NOT EXISTS `robot` (`id` BIGINT AUTO_INCREMENT PRIMARY KEY, `robot_id` VARCHAR(40) NOT NULL DEFAULT '', `token` VARCHAR(100) NOT NULL DEFAULT '', `version` BIGINT NOT NULL DEFAULT 0, `status` SMALLINT NOT NULL DEFAULT 1, `creator_uid` VARCHAR(40) NOT NULL DEFAULT '', `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, UNIQUE KEY `robot_id_robot_index` (`robot_id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci")
	}

	os.Exit(m.Run())
}
