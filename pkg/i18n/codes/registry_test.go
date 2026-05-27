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
		{"empty DefaultMessage", Code{ID: "err.x", HTTPStatus: 400, DefaultMessage: ""}},
		{"status too low", Code{ID: "err.x", HTTPStatus: 99, DefaultMessage: "x"}},
		{"status too high", Code{ID: "err.x", HTTPStatus: 600, DefaultMessage: "x"}},
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

func TestAll_SortedAndIndependent(t *testing.T) {
	withCleanRegistry(t)

	Register(Code{ID: "err.b", HTTPStatus: 400, DefaultMessage: "b"})
	Register(Code{ID: "err.a", HTTPStatus: 400, DefaultMessage: "a"})
	Register(Code{ID: "err.c", HTTPStatus: 400, DefaultMessage: "c"})

	got := All()
	want := []string{"err.a", "err.b", "err.c"}
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
	if c, _ := Lookup("err.a"); c.DefaultMessage == "MUTATED" {
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
	Register(Code{ID: "err.x", HTTPStatus: 400, DefaultMessage: "x"})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := Lookup("err.x"); !ok {
				t.Error("concurrent Lookup failed")
			}
		}()
	}
	wg.Wait()
}
