package cardtmpl_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

// TestMetaImmutable_CallerCannotPolluteRegistry 验证 R3-2:Template.Meta() 与
// Registry.List() 返回防御性深拷贝 —— 调用方修改返值 (Views map / ActionContract
// 指针) 不会污染 Registry 内部,再次读取仍是原值。冻结后契约不可变、并发读安全。
func TestMetaImmutable_CallerCannotPolluteRegistry(t *testing.T) {
	r := freshRegistry(t)

	var tmpl cardtmpl.Template
	for _, tp := range r.RegisteredForTest() {
		if tp.Meta().ID == docsaccessrequest.TemplateID {
			tmpl = tp
			break
		}
	}
	if tmpl == nil {
		t.Fatalf("pilot template not registered")
	}

	// 拿一份 Meta,尝试就地改坏它的 map/指针。
	m1 := tmpl.Meta()
	if m1.ActionContract == nil {
		t.Fatalf("pilot ActionContract nil")
	}
	m1.ActionContract.Owner = "HACKED"
	for k := range m1.Views {
		m1.Views[k] = cardtmpl.ViewSpec{WireProfile: "HACKED"}
	}

	// 再取一份,必须仍是原始值 —— 证明 m1 是独立拷贝,内部未被污染。
	m2 := tmpl.Meta()
	if m2.ActionContract.Owner != "docs" {
		t.Errorf("Meta().ActionContract polluted: got Owner=%q, want docs", m2.ActionContract.Owner)
	}
	vs, ok := m2.Views[cardtmpl.ViewKey(docsaccessrequest.StatePending)]
	if !ok {
		t.Fatalf("pending view missing after mutation attempt")
	}
	if vs.WireProfile != "octo/v2" {
		t.Errorf("Meta().Views polluted: got WireProfile=%q, want octo/v2", vs.WireProfile)
	}
	// ViewFor 走内部 stateIndex,也不应被污染。
	if _, ok := m2.ViewFor(docsaccessrequest.StatePending); !ok {
		t.Errorf("ViewFor(pending) broke after mutation attempt")
	}

	// List() 出口同样深拷贝。
	for _, lm := range r.List() {
		if lm.ID != docsaccessrequest.TemplateID {
			continue
		}
		lm.ActionContract.Owner = "HACKED2"
		for k := range lm.Views {
			lm.Views[k] = cardtmpl.ViewSpec{WireProfile: "HACKED2"}
		}
	}
	m3 := tmpl.Meta()
	if m3.ActionContract.Owner != "docs" {
		t.Errorf("List() mutation leaked into registry: Owner=%q", m3.ActionContract.Owner)
	}
}

// TestRender_HonorsCancelledContext 验证 R2-7:renderCore 把 caller ctx 传给
// Template.Build,pilot Build 在 ctx 已取消时早退。产物是 ErrRenderFailed
// (Build 错经 renderCore step4 包裹),错误信息带 "context canceled"。
func TestRender_HonorsCancelledContext(t *testing.T) {
	r := freshRegistry(t)
	sample, err := docsaccessrequest.Assets.ReadFile(
		docsaccessrequest.HandoffRoot + "/samples/pending.json",
	)
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	env := cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "zh-CN", SpaceID: "s1"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	_, err = r.Render(ctx, docsaccessrequest.TemplateID, "", docsaccessrequest.StatePending, sample, env)
	if err == nil {
		t.Fatalf("want error on cancelled ctx, got nil")
	}
	if !errors.Is(err, cardtmpl.ErrRenderFailed) {
		t.Errorf("want ErrRenderFailed, got %v", err)
	}
	// R4-2: renderCore 用 %w 包裹 Build err,errors.Is 必须能穿透探到 context.Canceled。
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want errors.Is(err, context.Canceled) true (ctx chain preserved), got %v", err)
	}

	// sanity:未取消时同样输入必须成功,证明失败确由 ctx 取消所致。
	if _, err := r.Render(context.Background(),
		docsaccessrequest.TemplateID, "", docsaccessrequest.StatePending, sample, env); err != nil {
		t.Fatalf("baseline (uncancelled) Render failed: %v", err)
	}
}

// TestAbsoluteHTTPSURL 验证 R4-3:绝对-https 单一真源。除 scheme/空 host 外,
// 还要拒绝 "https://:443"(纯端口无 hostname,旧 u.Host!="" 会放过)与 userinfo。
func TestAbsoluteHTTPSURL(t *testing.T) {
	ok := []string{
		"https://cdn.example.com/a.png",
		"https://host:8443/x",
		"https://a.b.c",
	}
	bad := []string{
		"",                     // 空
		"http://ok.com",        // 非 https
		"ftp://x",              // 非 https
		"https://",             // 无 host
		"https:///path",        // 无 host(首字符斜杠)
		"https://?q=1",         // host 空(query 紧跟)
		"https://#frag",        // host 空(fragment 紧跟)
		"https://:443/x",       // 纯端口无 hostname —— R4-3 收紧点
		"https://user@host/x",  // userinfo —— R4-3 拒绝
		"https://user:pw@host", // userinfo —— R4-3 拒绝
	}
	for _, u := range ok {
		if err := cardtmpl.AbsoluteHTTPSURL(u); err != nil {
			t.Errorf("AbsoluteHTTPSURL(%q) = %v, want nil", u, err)
		}
	}
	for _, u := range bad {
		if err := cardtmpl.AbsoluteHTTPSURL(u); err == nil {
			t.Errorf("AbsoluteHTTPSURL(%q) = nil, want error", u)
		}
	}
}
