package user

import (
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
)

func userAvatarFilePath(uid string, partition int, version int64) string {
	avatarID := crc32.ChecksumIEEE([]byte(uid)) % uint32(partition)
	if version > 0 {
		return fmt.Sprintf("avatar/%d/%s/%d.png", avatarID, uid, version)
	}
	return fmt.Sprintf("avatar/%d/%s.png", avatarID, uid)
}

// avatarETag 为本地生成的默认头像算一个 ETag。parts 应覆盖所有决定图像内容的
// 因子（渲染版本、uid→颜色、展示文字）——昵称变化时 ETag 随之变化，使共享缓存/
// 浏览器在改名后能 revalidate 拿到新图，而不是按 max-age 继续返回旧昵称头像。
//
// 这是**弱 ETag**（带 W/ 前缀）：指纹来自渲染输入而非响应字节，且渲染版本前缀
// 已能在渲染逻辑变更时使其失效，弱验证符语义更准确。crc32 仅作轻量指纹（非安全用途）。
func avatarETag(parts ...string) string {
	h := crc32.NewIEEE()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf(`W/"%08x"`, h.Sum32())
}

// avatarCacheKey 为进程级共享渲染缓存（avatarrender.Cache）算 key。parts 与
// avatarETag 取同一组决定图像内容的因子（渲染版本、uid→颜色、展示文字）。
//
// 关键：这里**不能**复用 avatarETag 的 CRC32 摘要。avatarETag 是 32 位 CRC32，
// 作弱 ETag 没问题（HTTP 304 撞了顶多多渲一次，无害）；但缓存 key 是**跨所有用户
// 的进程级 []byte 存储身份**，一旦碰撞就会把 A 用户已缓存的头像返回给 B 用户
// （串图 / 轻度信息泄露）。而且 text=昵称末两字用户可控、CRC32 线性可构造，可被
// 对抗性投毒。故 key 编码完整原始 parts 而非摘要。详见 PR#481 评审。
//
// 编码用**长度分帧**（每段 `<len>:<bytes>`）而非 NUL 分隔：后者对任意 parts 非
// 单射（如 ["u","a\x00b"] 与 ["u\x00a","b"] 会撞）。今天的因子（uid 来自 HTTP path
// 不含 NUL、text 经 IndividualText 已剥离控制字符）即便用 NUL 也安全，但长度分帧对
// **任意**字节都单射，避免将来新增可能含 NUL 的因子（或 #478 群头像接入同一 key
// 构造）时踩雷（PR#481 评审）。
func avatarCacheKey(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	return b.String()
}

// ifNoneMatchSatisfied 报告 If-None-Match 头是否匹配 etag（RFC 7232 §3.2 弱比较：
// 忽略 W/ 前缀）。支持逗号分隔的多个验证符与通配 "*"。
func ifNoneMatchSatisfied(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	target := etagOpaqueTag(etag)
	for _, tok := range strings.Split(header, ",") {
		if etagOpaqueTag(tok) == target {
			return true
		}
	}
	return false
}

// etagOpaqueTag 去掉 W/ 弱前缀和外层引号，返回不透明标签，用于弱比较。
func etagOpaqueTag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	return strings.Trim(s, `"`)
}
