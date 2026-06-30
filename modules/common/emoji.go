package common

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/avatarrender"
)

// emojiManifestJSON 是内置自定义表情清单的**唯一真源**(go:embed)。改动这份 JSON
// (新增/改名/调序/换图)即改动清单,务必同时把 version 自增 —— 客户端按 version 判断
// 是否需要刷新本地缓存。Unicode 标准表情(😀 那套)不在此处:它们标准、不变、各端本地
// 渲染即可,只有 [xxx] 这套自定义表情需要服务端下发。
//
//go:embed emojis/manifest.json
var emojiManifestJSON []byte

// emojiItem 是清单中一个自定义表情的对外(JSON)形式,字段下划线命名。
//   - Key  消息正文里的字面 token,如 "[使命必达]"。这是 wire 格式,各端据此匹配/渲染,
//     必须与历史 token 逐字节一致(老消息、老客户端都依赖它),不可改名。
//   - Name 人类可读标签(如 "使命必达"),供选择器 title / 无障碍文本使用。
//   - URL  图片地址。内置表情留空("")—— 客户端复用其已打包的本地图;后续在服务端新增的
//     表情会带非空 URL(对象存储/file 模块),客户端据此直接渲染,无需发版打包新图。
type emojiItem struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// emojiManifestResp 是 GET /v1/common/emojis 的响应:内置自定义表情清单的对外契约。
// 与未来 DB 版(Option A)契约一致,换源不换约。
type emojiManifestResp struct {
	Version int         `json:"version"` // 清单版本,任意改动需自增;客户端据此判定缓存是否失效
	List    []emojiItem `json:"list"`    // 按真源顺序排列
}

var (
	emojiManifestOnce  sync.Once
	emojiManifestValue emojiManifestResp
	emojiManifestETag  string
)

// loadEmojiManifest 解析内置清单并预计算其内容相关弱 ETag(只做一次)。New() 在启动期会主动
// 调用一次做 fail-fast:内嵌 JSON 损坏属编译进二进制的资产 bug,直接 panic 暴露(与 common
// 启动期其它资产校验一致),不进入运行时错误分支 —— 故此特性无需注册新的 errcode / i18n 文案。
// handler 也会调用它,首次之后是 no-op。
func loadEmojiManifest() {
	emojiManifestOnce.Do(func() {
		m, err := parseEmojiManifest(emojiManifestJSON)
		if err != nil {
			panic(fmt.Errorf("common: invalid embedded emoji manifest: %w", err))
		}
		emojiManifestValue = m
		// 内容相关弱 ETag:把 version + 每个条目的 key/name/url 都作为因子,清单任一改动即
		// ETag 变 → 已缓存客户端 revalidate 到新清单(不会被 max-age 钉死旧表)。
		parts := make([]string, 0, len(emojiManifestValue.List)*3+1)
		parts = append(parts, "emoji-manifest-v"+strconv.Itoa(emojiManifestValue.Version))
		for _, e := range emojiManifestValue.List {
			parts = append(parts, e.Key, e.Name, e.URL)
		}
		emojiManifestETag = avatarrender.ETag(parts...)
	})
}

// parseEmojiManifest 解析内嵌清单并做语义校验(纯函数,便于单测)。除 JSON 合法外还要求:
// version>=1、list 非空、每个 key 是 [xxx] token、key 全局唯一、name 非空。任一不满足即返回
// error,由 loadEmojiManifest 在启动期 panic —— 即便将来 manifest 被改坏且测试被弱化,也能
// fail-fast,守住"无运行时错误分支"的前提。URL 允许为空(内置表情复用本地图),不在校验之列。
func parseEmojiManifest(data []byte) (emojiManifestResp, error) {
	var m emojiManifestResp
	if err := json.Unmarshal(data, &m); err != nil {
		return emojiManifestResp{}, fmt.Errorf("parse json: %w", err)
	}
	if m.Version < 1 {
		return emojiManifestResp{}, fmt.Errorf("version must be >= 1, got %d", m.Version)
	}
	if len(m.List) == 0 {
		return emojiManifestResp{}, errors.New("list must not be empty")
	}
	seen := make(map[string]struct{}, len(m.List))
	for i, e := range m.List {
		if !isEmojiToken(e.Key) {
			return emojiManifestResp{}, fmt.Errorf("list[%d] key %q must be a [xxx] token", i, e.Key)
		}
		if _, dup := seen[e.Key]; dup {
			return emojiManifestResp{}, fmt.Errorf("duplicate key %q", e.Key)
		}
		seen[e.Key] = struct{}{}
		if e.Name == "" {
			return emojiManifestResp{}, fmt.Errorf("list[%d] (%s) has empty name", i, e.Key)
		}
	}
	return m, nil
}

// isEmojiToken 报告 s 是否为 [xxx] 形式的消息正文 token:以 [ 开头、] 结尾,中间非空且不含 ]。
func isEmojiToken(s string) bool {
	return len(s) >= 3 && s[0] == '[' && s[len(s)-1] == ']' && !strings.Contains(s[1:len(s)-1], "]")
}

// emojiManifest 返回内置自定义表情清单(公开、无鉴权)。客户端启动时拉取并按 version 缓存,
// 据此动态重建 token→图 的映射与渲染正则,取代各端硬编码的内置表情表 —— 新增/改动内置表情
// 只需改服务端真源,客户端无需各自发版。token 仍是 [xxx](消息正文 wire 格式),保持不变。
//
// 公开(不鉴权):清单为非敏感的全局静态数据,不涉及任何用户数据或 Space,且需在登录前即可用
// (登录/预览态也会渲染表情);与同模块已公开的 /v1/common/countries、/changelog 及
// /v1/group/avatar_palette 同级,故无需 AuthMiddleware / Space 中间件。亦无需 Strict 限流:
// 内容静态、可 304 缓存,全局 per-IP 限流(main.go)已是底线。
//
// 缓存:内容相关弱 ETag + must-revalidate,清单改动即失效;If-None-Match 命中返回 304。
func (cn *Common) emojiManifest(c *wkhttp.Context) {
	loadEmojiManifest()
	c.Header("ETag", emojiManifestETag)
	c.Header("Cache-Control", "public, max-age=300, must-revalidate")
	if avatarrender.IfNoneMatch(c.GetHeader("If-None-Match"), emojiManifestETag) {
		c.Status(http.StatusNotModified)
		return
	}
	c.Response(emojiManifestValue)
}
