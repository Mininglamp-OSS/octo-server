package searchetl

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

// 模块注册（YUJ-4530 阶段 1 骨架 → YUJ-5012 票4a 挂生命周期）。
//
// 阶段 1：仅注册迁移（建独立游标表 octo_etl_es_cursor），不启动 scheduler、不接 Kafka。
// 票4a（安全 checkpoint）：挂 scheduler 生命周期（Start/Stop），让这套「从未在生产真跑过」
// 的代码第一次真跑——**仅慢游标一个分钟级 tick**，不挂快游标（票4b）、不改 minTickSeconds clamp。
// Kafka.On 惰性接线保持不变（off 时 scheduler 按护栏跳过、RunIncremental 短路，零开销）。
// 生产启用（真开 Kafka.On 的部署动作）仍由 deploy 流程单独拍板，不在本票。
func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		appCtx := ctx.(*config.Context)
		etl := NewETL(appCtx)
		sched := newScheduler(appCtx, etl)
		return register.Module{
			Name:   "searchetl",
			SQLDir: register.NewSQLFS(sqlFS),
			// Service 暴露 ETL，便于后续 ops 端点/测试触达。
			Service: etl,
			// Start/Stop 走 octo-lib module 生命周期：Start 在 server 启动时调用，返回 error
			// 即 fatal 拒启动（慢游标 lag>0 + MySQL-only 护栏据此把人盯变代码挡）。
			Start: sched.Start,
			Stop: func() error {
				sched.Stop()
				return nil
			},
		}
	})
}
