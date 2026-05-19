package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// subHash 返回 OIDC sub claim 的 SHA-256 短哈希(前 8 个 hex 字符)。
//
// 用途:日志和审计 uid 前缀都用它替代明文截断。明文 sub 可能含 IdP 内部用户 id /
// 邮箱形式标识,落到日志或 audit.uid 都属于过度暴露;短哈希保留了"可关联同一
// 用户多次登录"的运维价值,又不可逆推回 IdP 用户身份。
//
// 8 hex(32 bit)冲突空间 ~4.3 亿,排查单条日志足够;真要审计请走 user_oidc_identity 表。
func subHash(sub string) string {
	if sub == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sub))
	return hex.EncodeToString(sum[:4])
}

// identifierKeyHash 给 SR-2.2 反枚举计数器(bind:enumfail:<hash>)用的哈希维度。
//
// 与 subHash 区分是因为后者只服务于日志关联(32-bit 即可),而本函数被攻击者
// 直接用作"输入 identifier → 计数器 key"的映射。32-bit 在 keyspace 维度可被
// 离线 pre-image 攻击:攻击者算 SHA-256 找一个与真实 username 前 4 字节冲突
// 的 identifier,从而共享同一锁定预算 / 反推真实用户名是否存在。
//
// 64-bit (16 hex) 把生日攻击成本提到 ~2^32 次哈希,显著高于 5 分钟 token
// 窗口内可达的吞吐。仍然非保密哈希,需要更强保证时改 HMAC keyed by
// DM_OIDC_RT_ENC_KEY,本期不引入额外密钥依赖。
func identifierKeyHash(identifier string) string {
	if identifier == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(identifier))
	return hex.EncodeToString(sum[:8])
}

// newTraceID 生成 16 hex(8 字节)随机 trace id,贯穿单次 callback 的所有日志。
//
// 不入 wkhttp middleware:OIDC callback 由 IdP 重定向命中,没有上游 X-Request-Id
// 来源,且本期只有 OIDC 路径需要,在入口本地生成更轻量。后续基础设施层补全
// middleware 后,可改为优先读 c.Request.Header.Get("X-Request-Id")。
//
// 失败兜底:crypto/rand 在标准化 OS 上几乎不会失败;真失败时返回固定占位串
// "0000000000000000",日志关联会丢但不阻塞登录。
func newTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}
