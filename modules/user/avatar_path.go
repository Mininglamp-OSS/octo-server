package user

import (
	"fmt"
	"hash/crc32"
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
