package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// StateData OIDC authorize → callback 之间的临时状态(/authorize 时写入,/callback 时一次性消费)
type StateData struct {
	Provider     string    `json:"provider"`
	CodeVerifier string    `json:"code_verifier"`
	Nonce        string    `json:"nonce"`
	IP           string    `json:"ip"`
	UserAgent    string    `json:"user_agent"`
	ReturnTo     string    `json:"return_to"`
	CreatedAt    time.Time `json:"created_at"`

	// ClientAuthcode 前端在 /authorize 阶段传入的短码,callback 完成后用作 ThirdAuthcode
	// Redis key 的后半段,前端按 GitHub 模式短码轮询取 LoginRespJSON。
	ClientAuthcode string `json:"client_authcode,omitempty"`
	// DeviceFlag 调用方期望的设备类型,透传给 IssueSession。
	// 取值与 octo-lib config.DeviceFlag 一致(iota 顺序):0=APP / 1=Web / 2=PC。
	DeviceFlag uint8 `json:"device_flag,omitempty"`
}

// StateStore 状态存储抽象,生产用 Redis,测试用内存
type StateStore interface {
	Save(ctx context.Context, state string, data *StateData, ttl time.Duration) error
	// Consume 取出并删除,实现 CSRF 防护的一次性语义
	Consume(ctx context.Context, state string) (*StateData, error)
}

// ErrStateNotFound state 已过期、被消费或从未存在
var ErrStateNotFound = errors.New("oidc: state not found or already consumed")

// ---------- memory impl (用于单测) ----------
//
// 仅供单元测试使用。生产路径走 redisStateStore,Redis 自带 TTL 自动过期。
// memory impl 不含后台 GC goroutine — 未消费的过期条目会持续驻留直到进程结束;
// 如需用于本地开发等长期运行场景,需自行加扫描清理逻辑。

type memoryStateStore struct {
	mu   sync.Mutex
	data map[string]memoryEntry
}

type memoryEntry struct {
	value     *StateData
	expiresAt time.Time
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{data: make(map[string]memoryEntry)}
}

func (m *memoryStateStore) Save(_ context.Context, state string, data *StateData, ttl time.Duration) error {
	if state == "" {
		return errors.New("oidc: state key required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *data
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	m.data[state] = memoryEntry{value: &cp, expiresAt: time.Now().Add(ttl)}
	return nil
}

func (m *memoryStateStore) Consume(_ context.Context, state string) (*StateData, error) {
	if state == "" {
		return nil, ErrStateNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.data[state]
	if !ok || time.Now().After(entry.expiresAt) {
		// 顺手 GC 已过期但未消费的条目;对调用方而言"过期"与"不存在"语义等价
		delete(m.data, state)
		return nil, ErrStateNotFound
	}
	delete(m.data, state)
	return entry.value, nil
}

// ---------- helpers (RFC 7636 PKCE + 通用随机字符串) ----------

// NewRandomString 生成 URL-safe Base64 编码的随机字符串(byteLen 字节熵)
func NewRandomString(byteLen int) (string, error) {
	if byteLen <= 0 {
		return "", fmt.Errorf("oidc: byteLen must be > 0")
	}
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oidc: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewPKCEPair 按 RFC 7636 S256 生成 (code_verifier, code_challenge)
func NewPKCEPair() (verifier, challenge string, err error) {
	verifier, err = NewRandomString(64) // 86 字符 base64,落在 43~128 区间
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// ---------- json codec(供 redis impl 复用) ----------

// encodeStateData 编码为 JSON;若 CreatedAt 为零值用当前时间补齐。
// 内部对 d 做浅拷贝,避免修改调用方传入的指针(否则 redis 实现的 Save
// 会被观察到副作用,与 memory 实现的语义不一致)。
func encodeStateData(d *StateData) (string, error) {
	cp := *d
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	b, err := json.Marshal(&cp)
	if err != nil {
		return "", fmt.Errorf("oidc: marshal state: %w", err)
	}
	return string(b), nil
}

func decodeStateData(s string) (*StateData, error) {
	var d StateData
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return nil, fmt.Errorf("oidc: unmarshal state: %w", err)
	}
	return &d, nil
}
