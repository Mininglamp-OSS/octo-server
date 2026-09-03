package oidc

import "testing"

// 端点限流用的 Redis client 必须登记到 Close(),否则每次构造都漏一个连接池。
//
// routeAt 会被调用**两次**:一次用配置的 provider ID,一次用 legacy 路径 ID
// (见 Route)。所以每次装路由就会构造两个 instrumented client。本模块其他所有
// client(bindStore、idTokens、token invalidator 的 compare-deleter)都在 Close()
// 里被释放,只有这两个没有 —— 与文件自身的既有约定不一致。
//
// 量级有界(启动期两个),但"有界的泄漏"在测试环境里会被反复放大:每个
// testutil.NewTestServer 都会装一遍路由。
func TestExchangeLimiterClients_AreTrackedForClose(t *testing.T) {
	o := &OIDC{}
	if len(o.exchangeLimiterClients) != 0 {
		t.Fatalf("a fresh OIDC must track no limiter clients, got %d",
			len(o.exchangeLimiterClients))
	}

	// 模拟 routeAt 的两次构造。
	o.trackExchangeLimiterClient(nil)
	o.trackExchangeLimiterClient(nil)
	if got := len(o.exchangeLimiterClients); got != 2 {
		t.Fatalf("tracked %d clients, want 2 (routeAt runs once per path id)", got)
	}

	// closeExchangeLimiterClients 必须清空列表,这样重复 Close() 不会二次关闭。
	if err := o.closeExchangeLimiterClients(); err != nil {
		t.Fatalf("closeExchangeLimiterClients: %v", err)
	}
	if got := len(o.exchangeLimiterClients); got != 0 {
		t.Errorf("after close, %d clients still tracked; a second Close() would "+
			"double-close them", got)
	}
	// 幂等:再关一次不能报错。
	if err := o.closeExchangeLimiterClients(); err != nil {
		t.Errorf("second close must be a no-op, got %v", err)
	}
}
