package cardtmpl

import (
	"sync"
)

// defaultRegistry 是进程内单一 Registry。main.go composition root 通过
// SetDefaultRegistry 注入构造好的 Registry(注册所有 L2a Template + Freeze)。
// 它只供启动期 registration/static reconciliation 和兼容测试使用；生产运行期
// consumer 统一通过 DefaultCatalog() 读取，避免绕过 dynamic block/active 真相。
//
// 采用 pkg-scoped default 而非全局单例是权衡:
//   - test 环境可以 SetDefaultRegistry(nil) 后自己构造独立 Registry;
//   - 生产 main.go 只调一次 SetDefaultRegistry,之后 DefaultRegistry() 无锁并发读。
var (
	defaultRegistryMu sync.RWMutex
	defaultRegistry   *Registry
)

// SetDefaultRegistry 由 composition root 一次性调用,注入注册完成 + Freeze 的 Registry。
// 传 nil 用于 test 隔离。重复调用允许(后者覆盖),但生产 wiring 不应发生。
func SetDefaultRegistry(r *Registry) {
	defaultRegistryMu.Lock()
	defer defaultRegistryMu.Unlock()
	defaultRegistry = r
}

// DefaultRegistry 返回启动期 built-in Registry。未注入 → nil。
func DefaultRegistry() *Registry {
	defaultRegistryMu.RLock()
	defer defaultRegistryMu.RUnlock()
	return defaultRegistry
}
