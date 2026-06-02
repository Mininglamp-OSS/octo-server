// Package search · 图文混排 RichText(=14) 搜索索引单测。
//
// 任务背景：原本只搜 payload.content / payload.name，RichText(=14) 的正文文字在
// content blocks 内、镜像在顶层 plain，content 命不中。这里锁定搜索 payload 与
// highlights 都补上 payload.plain。
package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildMessageSearchQuery_IncludesPlain(t *testing.T) {
	payload, highlights := buildMessageSearchQuery("北京")

	// payload 匹配字段必须含 plain，且与 content/name 取同一关键字。
	assert.Equal(t, "北京", payload["plain"])
	assert.Equal(t, "北京", payload["content"])
	assert.Equal(t, "北京", payload["name"])

	// highlights 必须含 payload.plain，保证富文本命中后高亮回显。
	assert.Contains(t, highlights, "payload.plain")
	assert.Contains(t, highlights, "payload.content")
	assert.Contains(t, highlights, "payload.name")
}
