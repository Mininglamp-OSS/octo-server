package space

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"go.uber.org/zap"
)

// DefaultCategoryProvisioner 保证 (uid, spaceID) 下默认分类存在的钩子。
//
// 为什么用 hook 而不是直接 import：modules/category 的测试通过 blank import
// 拉入 modules/space 进行迁移注册；若 modules/space 反过来 import
// modules/category，即构成测试期 import cycle。此处由 category 在 init 中反向
// 注入 provisioner，两模块生产代码单向依赖（category → space），cycle 消除。
//
// 调用方需把任何错误视作非关键路径（降级为 warn），因为：
//   * category.EnsureDefaultCategory 内部依赖表唯一索引 + INSERT IGNORE，
//     重复 / 并发调用天然幂等；
//   * GET /v1/spaces/{id}/categories 端有兜底补偿；
//   * 生产 flow（创建/加入空间）绝不能因 category 初始化失败而回滚。
type DefaultCategoryProvisioner func(ctx *config.Context, uid, spaceID string) error

var defaultCategoryProvisioner DefaultCategoryProvisioner

// RegisterDefaultCategoryProvisioner 由 modules/category 的 init 调用完成注册。
// 覆盖允许（latest wins），方便测试替身。nil 时等价于取消注册。
func RegisterDefaultCategoryProvisioner(fn DefaultCategoryProvisioner) {
	defaultCategoryProvisioner = fn
}

// ensureDefaultCategoryProvisioned 在保持 nil-safe 的前提下调用已注册的 provisioner。
// logger 用于把错误降级为 warn；当 provisioner 未注册时静默返回。
func ensureDefaultCategoryProvisioned(ctx *config.Context, uid, spaceID string, logger interface {
	Warn(msg string, fields ...zap.Field)
}) {
	fn := defaultCategoryProvisioner
	if fn == nil {
		return
	}
	if err := fn(ctx, uid, spaceID); err != nil && logger != nil {
		logger.Warn("初始化默认分类失败", zap.Error(err), zap.String("uid", uid), zap.String("spaceId", spaceID))
	}
}
