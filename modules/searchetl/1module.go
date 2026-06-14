package searchetl

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

// 模块注册（YUJ-4530 阶段 1 骨架）。
//
// 阶段 1 只注册迁移（建独立游标表 octo_etl_es_cursor）。**不**启动 scheduler、**不**接 Kafka——
// producer 的事务拆分 / 单副本互斥 / Kafka 投递在阶段 2/3 落地，届时再在此挂 Start/Stop 钩子。
// 这样阶段 1 PR 可独立编译/迁移 dry-run，且不在生产引入任何运行期行为变更。
func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		appCtx := ctx.(*config.Context)
		return register.Module{
			Name:   "searchetl",
			SQLDir: register.NewSQLFS(sqlFS),
			// Service 暴露 ETL，便于后续 ops 端点/测试触达（阶段 1 仅 dry-run 能力）。
			Service: NewETL(appCtx),
		}
	})
}
