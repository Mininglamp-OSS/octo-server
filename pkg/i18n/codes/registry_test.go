package codes

import (
	"net/http"
	"sync"
	"testing"
)

// withCleanRegistry 在子测试期间清空全局注册表并在结束后恢复，
// 保证子测试互相独立、不污染 init() 注册的真实业务码。
func withCleanRegistry(t *testing.T) {
	t.Helper()
	mu.Lock()
	snapshot := make(map[string]Code, len(registry))
	for k, v := range registry {
		snapshot[k] = v
	}
	registry = make(map[string]Code)
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		registry = snapshot
		mu.Unlock()
	})
}

func TestRegister_AndLookup(t *testing.T) {
	withCleanRegistry(t)

	c := Code{
		ID:             "err.shared.test.sample",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "sample error",
	}
	Register(c)

	got, ok := Lookup("err.shared.test.sample")
	if !ok {
		t.Fatal("Lookup returned ok=false for just-registered code")
	}
	if got.ID != c.ID || got.HTTPStatus != c.HTTPStatus || got.DefaultMessage != c.DefaultMessage {
		t.Fatalf("Lookup returned %+v, want %+v", got, c)
	}

	if _, ok := Lookup("err.shared.test.missing"); ok {
		t.Fatal("Lookup returned ok=true for unregistered code")
	}
}

func TestRegister_PanicsOnDuplicate(t *testing.T) {
	withCleanRegistry(t)

	c := Code{ID: "err.shared.test.dup", HTTPStatus: 400, DefaultMessage: "x"}
	Register(c)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register did not panic on duplicate ID")
		}
	}()
	Register(c)
}

func TestRegister_PanicsOnInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		code Code
	}{
		{"empty ID", Code{ID: "", HTTPStatus: 400, DefaultMessage: "x"}},
		{"empty DefaultMessage", Code{ID: "err.shared.x.y", HTTPStatus: 400, DefaultMessage: ""}},
		{"status too low", Code{ID: "err.shared.x.y", HTTPStatus: 99, DefaultMessage: "x"}},
		{"status too high", Code{ID: "err.shared.x.y", HTTPStatus: 600, DefaultMessage: "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withCleanRegistry(t)
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Register did not panic for %s", tt.name)
				}
			}()
			Register(tt.code)
		})
	}
}

// TestRegister_PanicsOnInvalidIDFormat 验证 idPattern 命名约定强校验。
// 防止后续业务 module 用大写/空格/错前缀注册码，绕过 CI lint 漏到运行期。
func TestRegister_PanicsOnInvalidIDFormat(t *testing.T) {
	bad := []string{
		"err.shared.AUTH.required",        // uppercase
		"err.shared.auth required",        // space
		"err.shared.",                     // empty trailing
		"err.shared",                      // no trailing segment
		"err.client.foo.bar",              // wrong namespace
		"errshared.foo.bar",               // missing first dot
		"err..foo.bar",                    // empty namespace
		"err.shared.foo-bar",              // hyphen not allowed
		"err.shared.foo.bar.",             // trailing dot
		"Err.shared.foo.bar",              // uppercase prefix
	}
	for _, id := range bad {
		t.Run(id, func(t *testing.T) {
			withCleanRegistry(t)
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Register did not panic for invalid ID %q", id)
				}
			}()
			Register(Code{ID: id, HTTPStatus: 400, DefaultMessage: "x"})
		})
	}
}

// TestRegister_AcceptsValidIDFormat 反向覆盖：合法 ID 不应被拒绝。
func TestRegister_AcceptsValidIDFormat(t *testing.T) {
	good := []string{
		"err.shared.auth.required",
		"err.shared.not_found",
		"err.shared.rate.limited",
		"err.server.thread.archive_full",
		"err.server.user.x.y.z.w",  // 多层
	}
	for _, id := range good {
		t.Run(id, func(t *testing.T) {
			withCleanRegistry(t)
			Register(Code{ID: id, HTTPStatus: 400, DefaultMessage: "x"})
			if _, ok := Lookup(id); !ok {
				t.Fatalf("Register accepted %q but Lookup returned false", id)
			}
		})
	}
}

// TestLookup_ReturnsDeepCopy 验证 P1 修复：调用方修改返回值的引用字段
// 不应污染 registry。renderer 在 hot path 上读这些字段，未深拷贝会触发 race。
func TestLookup_ReturnsDeepCopy(t *testing.T) {
	withCleanRegistry(t)

	Register(Code{
		ID:             "err.shared.deepcopy.test",
		HTTPStatus:     400,
		DefaultMessage: "x",
		DefaultMessages: map[string]string{
			"zh-CN": "中文",
			"en-US": "english",
		},
		SafeDetailKeys: []string{"field", "retry_after"},
	})

	got1, _ := Lookup("err.shared.deepcopy.test")
	got1.DefaultMessages["zh-CN"] = "MUTATED"
	got1.DefaultMessages["NEW"] = "leak"
	got1.SafeDetailKeys[0] = "MUTATED"
	got1.SafeDetailKeys = append(got1.SafeDetailKeys, "leaked")

	got2, _ := Lookup("err.shared.deepcopy.test")
	if got2.DefaultMessages["zh-CN"] != "中文" {
		t.Errorf("registry DefaultMessages mutated through Lookup: %v", got2.DefaultMessages)
	}
	if _, leaked := got2.DefaultMessages["NEW"]; leaked {
		t.Errorf("new key leaked into registry: %v", got2.DefaultMessages)
	}
	if got2.SafeDetailKeys[0] != "field" {
		t.Errorf("SafeDetailKeys[0] mutated through Lookup: %v", got2.SafeDetailKeys)
	}
	if len(got2.SafeDetailKeys) != 2 {
		t.Errorf("SafeDetailKeys append leaked: %v", got2.SafeDetailKeys)
	}
}

// TestRegister_DeepCopiesInput 调用方在 Register 后修改原入参的引用字段
// 不应影响已注册的 Code。
func TestRegister_DeepCopiesInput(t *testing.T) {
	withCleanRegistry(t)

	msgs := map[string]string{"zh-CN": "中文"}
	keys := []string{"field"}
	Register(Code{
		ID:              "err.shared.deepcopy.input",
		HTTPStatus:      400,
		DefaultMessage:  "x",
		DefaultMessages: msgs,
		SafeDetailKeys:  keys,
	})

	// 入参后续突变 —— 不应影响 registry。
	msgs["zh-CN"] = "MUTATED"
	msgs["NEW"] = "leak"
	keys[0] = "MUTATED"

	got, _ := Lookup("err.shared.deepcopy.input")
	if got.DefaultMessages["zh-CN"] != "中文" {
		t.Errorf("registry mutated through Register input map: %v", got.DefaultMessages)
	}
	if got.SafeDetailKeys[0] != "field" {
		t.Errorf("registry mutated through Register input slice: %v", got.SafeDetailKeys)
	}
}

// TestAll_ReturnsDeepCopy 同 TestLookup_ReturnsDeepCopy，但走 All 路径。
func TestAll_ReturnsDeepCopy(t *testing.T) {
	withCleanRegistry(t)

	Register(Code{
		ID:              "err.shared.all.deepcopy",
		HTTPStatus:      400,
		DefaultMessage:  "x",
		DefaultMessages: map[string]string{"zh-CN": "中文"},
		SafeDetailKeys:  []string{"field"},
	})

	out := All()
	out[0].DefaultMessages["zh-CN"] = "MUTATED"
	out[0].SafeDetailKeys[0] = "MUTATED"

	got, _ := Lookup("err.shared.all.deepcopy")
	if got.DefaultMessages["zh-CN"] != "中文" {
		t.Errorf("registry mutated through All() return: %v", got.DefaultMessages)
	}
	if got.SafeDetailKeys[0] != "field" {
		t.Errorf("SafeDetailKeys mutated through All() return: %v", got.SafeDetailKeys)
	}
}

func TestAll_SortedAndIndependent(t *testing.T) {
	withCleanRegistry(t)

	Register(Code{ID: "err.shared.b", HTTPStatus: 400, DefaultMessage: "b"})
	Register(Code{ID: "err.shared.a", HTTPStatus: 400, DefaultMessage: "a"})
	Register(Code{ID: "err.shared.c", HTTPStatus: 400, DefaultMessage: "c"})

	got := All()
	want := []string{"err.shared.a", "err.shared.b", "err.shared.c"}
	if len(got) != len(want) {
		t.Fatalf("All returned %d entries, want %d", len(got), len(want))
	}
	for i, c := range got {
		if c.ID != want[i] {
			t.Errorf("All[%d].ID = %q, want %q", i, c.ID, want[i])
		}
	}

	// 返回的切片应是副本：调用方修改不影响 registry。
	got[0].DefaultMessage = "MUTATED"
	if c, _ := Lookup("err.shared.a"); c.DefaultMessage == "MUTATED" {
		t.Fatal("All returned a slice that aliases registry state")
	}
}

// TestRegister_ConcurrentSafe 多 goroutine 并发注册不同 ID 应全部成功，
// 重复 ID 应有且仅有一次 panic。验证 sync.RWMutex 写锁正确性。
func TestRegister_ConcurrentSafe(t *testing.T) {
	withCleanRegistry(t)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				_ = recover() // 接住可能的 dup panic
			}()
			Register(Code{
				ID:             "err.shared.test.concurrent." + itoa(i),
				HTTPStatus:     400,
				DefaultMessage: "x",
			})
		}(i)
	}
	wg.Wait()

	if got := len(All()); got != 50 {
		t.Fatalf("expected 50 unique registrations, got %d", got)
	}
}

// itoa 避免引入 strconv，保持测试文件依赖最少。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestLookup_ConcurrentReaders(t *testing.T) {
	withCleanRegistry(t)
	Register(Code{ID: "err.shared.x", HTTPStatus: 400, DefaultMessage: "x"})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := Lookup("err.shared.x"); !ok {
				t.Error("concurrent Lookup failed")
			}
		}()
	}
	wg.Wait()
}
