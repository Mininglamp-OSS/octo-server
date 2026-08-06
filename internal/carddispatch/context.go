package carddispatch

import "errors"

const registryContextKey = "octo-server.internal.carddispatch.registry.v1"

type ValueStore interface {
	SetValue(value interface{}, key string)
	Value(key string) interface{}
}

// Install publishes one immutable registry into one application context. It is
// called only by the single-threaded bootstrap path before module construction.
func Install(ctx ValueStore, registry *Registry) error {
	if ctx == nil || registry == nil {
		return errors.New("carddispatch: registry install requires context and registry")
	}
	if ctx.Value(registryContextKey) != nil {
		return ErrRegistryAlreadyInstalled
	}
	ctx.SetValue(registry, registryContextKey)
	return nil
}

// ProducerBindingFromContext 解析已安装 Registry 的只读 producer 身份绑定
// （PR-C D3）。Registry 未安装或 producer 未注册一律 (\"\", false)，调用方
// 对 dynamic provenance fail-close。与 SenderFromContext 不同，它绝不返回
// 发送能力 —— 业务 caller 无法借它构造 ProducerSpec 或拿到 Sender。
func ProducerBindingFromContext(ctx ValueStore, id ProducerID) (string, bool) {
	if ctx == nil {
		return "", false
	}
	registry, ok := ctx.Value(registryContextKey).(*Registry)
	if !ok || registry == nil {
		return "", false
	}
	return registry.ProducerBinding(id)
}

func SenderFromContext(ctx ValueStore, id ProducerID) (Sender, error) {
	if ctx == nil {
		return nil, categorized(CategoryProducerDisabled, errors.New("application context unavailable"))
	}
	registry, ok := ctx.Value(registryContextKey).(*Registry)
	if !ok || registry == nil {
		return nil, categorized(CategoryProducerDisabled, errors.New("registry unavailable"))
	}
	return registry.Sender(id)
}
