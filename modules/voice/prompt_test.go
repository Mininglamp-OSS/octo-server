package voice

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildPrompt_NoContext(t *testing.T) {
	prompt := buildPrompt("", "")
	assert.Equal(t, transcribePrompt, prompt)
	assert.Contains(t, prompt, "准确转写为文字")
	assert.NotContains(t, prompt, "已有文本")
}

func TestBuildPrompt_WithContext(t *testing.T) {
	contextText := "Hello, this is existing text."
	prompt := buildPrompt(contextText, "")

	assert.Contains(t, prompt, "已有文本")
	assert.Contains(t, prompt, contextText)
	assert.Contains(t, prompt, "修改或补充")
	assert.NotEqual(t, transcribePrompt, prompt)
}

func TestBuildPrompt_ContextTextEmbedded(t *testing.T) {
	contextText := "Line 1\nLine 2\nLine 3"
	prompt := buildPrompt(contextText, "")

	// Context text should appear between the --- delimiters
	parts := strings.Split(prompt, "---")
	assert.True(t, len(parts) >= 3, "prompt should contain --- delimiters")
	assert.Contains(t, parts[1], contextText)
}

func TestBuildPrompt_WithChatContext_TranscribeMode(t *testing.T) {
	chatCtx := "Alice: 你好\nBob: 你好啊"
	prompt := buildPrompt("", chatCtx)

	assert.Contains(t, prompt, "以下是当前聊天的最近对话记录")
	assert.Contains(t, prompt, chatCtx)
	assert.Contains(t, prompt, "准确转写为文字")
	assert.NotContains(t, prompt, "已有文本")
}

func TestBuildPrompt_WithChatContext_ModifyMode(t *testing.T) {
	chatCtx := "Alice: 会议在周五\nBob: 收到"
	contextText := "existing text"
	prompt := buildPrompt(contextText, chatCtx)

	assert.Contains(t, prompt, "以下是当前聊天的最近对话记录")
	assert.Contains(t, prompt, chatCtx)
	assert.Contains(t, prompt, "已有文本")
	assert.Contains(t, prompt, contextText)
	assert.Contains(t, prompt, "修改或补充")

	// Chat context should appear before the main prompt
	chatCtxIdx := strings.Index(prompt, chatCtx)
	mainPromptIdx := strings.Index(prompt, "已有文本")
	assert.True(t, chatCtxIdx < mainPromptIdx, "chat context should precede the main prompt")
}

func TestBuildPrompt_EmptyChatContext(t *testing.T) {
	prompt := buildPrompt("", "")
	assert.NotContains(t, prompt, "以下是当前聊天的最近对话记录")
	assert.Equal(t, transcribePrompt, prompt)
}
