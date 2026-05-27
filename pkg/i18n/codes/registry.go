// Package codes 维护 i18n 错误码注册表。所有 user-visible 错误码都必须在此
// 通过 Register 登记，AST extractor 以 Register 为唯一数据源生成 goi18n
// message marker（D18）。
//
// 设计要点：
//   - ID 是稳定的 i18n key（如 "err.shared.auth.required"），全局唯一，
//     运行期通过 Lookup 检索；Register 检测到重复 ID 直接 panic（启动期问题）。
//   - DefaultMessage 是 source（en-US）文案，AST extractor 输出到 active.en-US.toml。
//   - DefaultMessages 仅作极端故障兜底（缺译 + go-i18n bundle 加载失败）；
//     正常运营依赖 bundle 里的 translate.<lang>.toml，不要塞业务文案到这里。
//   - SafeDetailKeys 是 details 字段白名单——renderer 渲染时只透传白名单内的 key，
//     防止业务层不小心把 uid/token/raw_err 泄露给客户端。
//   - Internal=true 标记 5xx 一类错误；renderer 看到 Internal=true 时输出
//     占位文案，避免内部 message 泄露给客户端（D11/D13）。
//
// 调用顺序约定：codes 包通过 init() 注册；errcode 包同样在 init() 注册业务码；
// init 顺序由 Go 包依赖图决定（registry → shared → server），重复注册 panic
// 保证开发期立刻发现冲突。
package codes

import (
	"fmt"
	"sort"
	"sync"
)

// Code 描述一个稳定 i18n 错误码及其元信息。
//
// 字段语义：
//   - ID:              全局唯一稳定 key，如 "err.shared.auth.required"。
//   - HTTPStatus:      canonical HTTP status（401/403/429/...）。兼容期内 renderer
//                      可能把响应头固定为 400（D14），但 error.http_status body 字段
//                      仍暴露真值。
//   - DefaultMessage:  source 文案（en-US），AST extractor 生成的 message marker
//                      用它做 Other 字段；运行期当 bundle 缺译时也用它做最后兜底。
//   - DefaultMessages: 极端故障兜底（bundle 加载失败 / 全部 lang 缺译时使用）。
//                      key 是 BCP-47 lang tag，如 "zh-CN"。
//   - SafeDetailKeys:  ResponseErrorL 调用方传入 details 时，renderer 只透传
//                      此列表内的 key；其余键被丢弃并记 i18n_unsafe_details_dropped_total。
//   - Internal:        5xx 类错误标记。renderer 看到时不输出 spec.DefaultMessage，
//                      改用「服务器内部错误」之类占位文案。
type Code struct {
	ID              string
	HTTPStatus      int
	DefaultMessage  string
	DefaultMessages map[string]string
	SafeDetailKeys  []string
	Internal        bool
}

var (
	mu       sync.RWMutex
	registry = make(map[string]Code)
)

// Register 登记一个错误码。重复 ID 直接 panic（启动期阻断，避免运行期歧义）。
// 空 ID 同样 panic。
//
// 典型用法：在包级 init() 内调用，所有业务码集中在 shared.go / server.go 等
// 显式文件，便于 AST extractor 读取。
func Register(c Code) {
	if c.ID == "" {
		panic("codes.Register: empty Code.ID")
	}
	if c.DefaultMessage == "" {
		panic(fmt.Sprintf("codes.Register: empty DefaultMessage for %q", c.ID))
	}
	if c.HTTPStatus < 100 || c.HTTPStatus > 599 {
		panic(fmt.Sprintf("codes.Register: invalid HTTPStatus %d for %q", c.HTTPStatus, c.ID))
	}

	mu.Lock()
	defer mu.Unlock()
	if _, ok := registry[c.ID]; ok {
		panic(fmt.Sprintf("codes.Register: duplicate ID %q", c.ID))
	}
	registry[c.ID] = c
}

// Lookup 按 ID 检索 Code，未注册返回 false。
// 运行期高频调用（每次 ResponseErrorL），用 RWMutex 读锁。
func Lookup(id string) (Code, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[id]
	return c, ok
}

// All 返回全部已注册 Code 的副本，按 ID 字典序排序。
// 主要供 AST extractor、CI lint、调试 dump 使用，不应在 hot path 调用。
func All() []Code {
	mu.RLock()
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	out := make([]Code, 0, len(registry))
	mu.RUnlock()

	sort.Strings(ids)
	mu.RLock()
	defer mu.RUnlock()
	for _, id := range ids {
		if c, ok := registry[id]; ok {
			out = append(out, c)
		}
	}
	return out
}

// reset 仅供测试使用。生产代码绝不应调用——registry 在 init 期定型后
// 不应在运行期清空，否则未注册 code 阻断（CI lint）失去意义。
func reset() {
	mu.Lock()
	defer mu.Unlock()
	registry = make(map[string]Code)
}
