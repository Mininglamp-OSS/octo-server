package voice

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- buildSystemMessage tests ---

func TestBuildSystemMessage_ReturnsSystemPrompt(t *testing.T) {
	msg := buildSystemMessage()
	assert.Equal(t, activePrompts.System, msg)
	assert.Contains(t, msg, "智能语音转写器")
	assert.Contains(t, msg, "[NO_SPEECH]")
	assert.Contains(t, msg, "vocabulary_reference")
	assert.Contains(t, msg, "input_buffer")
}

func TestBuildSystemMessage_ContainsAllRules(t *testing.T) {
	msg := buildSystemMessage()
	assert.Contains(t, msg, "无语音判定")
	assert.Contains(t, msg, "禁止猜测")
	assert.Contains(t, msg, "语言润色")
	assert.Contains(t, msg, "数据标签说明")
	assert.Contains(t, msg, "编辑指令识别")
	assert.Contains(t, msg, "追加新内容")
	assert.Contains(t, msg, "词汇参考表使用规则")
	assert.Contains(t, msg, "输出格式")
}

func TestBuildSystemMessage_ContainsExamples(t *testing.T) {
	msg := buildSystemMessage()
	assert.Contains(t, msg, "大背头")
	assert.Contains(t, msg, "托马斯")
	assert.Contains(t, msg, "嗯，好的，我知道了")
}

// --- buildUserMessage: default/transcribe mode ---

func TestBuildUserMessage_Default_NoContext(t *testing.T) {
	msg := buildUserMessage("", "", "")
	assert.Equal(t, taskTranscribe, msg)
	assert.Contains(t, msg, "请转写音频中的语音")
	assert.NotContains(t, msg, "vocabulary_reference")
	assert.NotContains(t, msg, "input_buffer")
}

func TestBuildUserMessage_Default_WithVocab(t *testing.T) {
	msg := buildUserMessage("", "", "张三、李四")
	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "张三、李四")
	assert.Contains(t, msg, "不要输出纠错上下文中的任何内容")
	assert.NotContains(t, msg, "input_buffer")
}

// --- buildUserMessage: edit mode ---

func TestBuildUserMessage_Edit_NoContext(t *testing.T) {
	msg := buildUserMessage("edit", "", "")
	assert.Equal(t, taskTranscribe, msg)
}

func TestBuildUserMessage_Edit_WithContextText(t *testing.T) {
	msg := buildUserMessage("edit", "existing text", "")
	assert.Contains(t, msg, "<input_buffer>")
	assert.Contains(t, msg, "existing text")
	assert.Contains(t, msg, "根据音频中的语音对其进行处理")
	assert.Contains(t, msg, "编辑指令")
	assert.NotContains(t, msg, "vocabulary_reference")
}

func TestBuildUserMessage_Edit_WithVocabOnly(t *testing.T) {
	msg := buildUserMessage("edit", "", "Alice: 测试")
	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "Alice: 测试")
	assert.Contains(t, msg, "不要输出纠错上下文中的任何内容")
	assert.NotContains(t, msg, "input_buffer")
}

func TestBuildUserMessage_Edit_WithBothContexts(t *testing.T) {
	msg := buildUserMessage("edit", "existing draft", "Alice: 专有名词ABC")

	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "Alice: 专有名词ABC")
	assert.Contains(t, msg, "<input_buffer>")
	assert.Contains(t, msg, "existing draft")
	assert.Contains(t, msg, "编辑指令")

	// vocabulary_reference should appear before input_buffer
	vocabIdx := strings.Index(msg, "<vocabulary_reference>")
	bufferIdx := strings.Index(msg, "<input_buffer>")
	assert.True(t, vocabIdx < bufferIdx, "vocabulary_reference should appear before input_buffer")

	// task instruction at the end
	taskIdx := strings.Index(msg, "请根据音频中的语音处理上述文本")
	assert.True(t, taskIdx > bufferIdx, "task instruction should appear after input_buffer")
}

// --- buildUserMessage: append mode ---

func TestBuildUserMessage_Append_NoContext(t *testing.T) {
	msg := buildUserMessage("append", "", "")
	assert.Equal(t, taskTranscribe, msg)
}

func TestBuildUserMessage_Append_WithContextText_NoVocab(t *testing.T) {
	msg := buildUserMessage("append", "已有的文本内容", "")
	assert.Contains(t, msg, "<input_buffer>")
	assert.Contains(t, msg, "已有的文本内容")
	assert.Contains(t, msg, "辅助你理解当前语境")
	assert.Contains(t, msg, "只输出音频中新听到的内容")
	assert.NotContains(t, msg, "vocabulary_reference")
	assert.NotContains(t, msg, "编辑指令")
}

func TestBuildUserMessage_Append_WithVocabOnly(t *testing.T) {
	msg := buildUserMessage("append", "", "Alice: 聊天内容")
	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "Alice: 聊天内容")
	assert.NotContains(t, msg, "input_buffer")
}

func TestBuildUserMessage_Append_WithBothContexts(t *testing.T) {
	msg := buildUserMessage("append", "原有文本", "Alice: 聊天")

	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "Alice: 聊天")
	assert.Contains(t, msg, "<input_buffer>")
	assert.Contains(t, msg, "原有文本")
	assert.Contains(t, msg, "配合vocabulary_reference纠正专有名词拼写")
	assert.Contains(t, msg, "只输出音频中新听到的内容")

	// vocabulary_reference before input_buffer
	vocabIdx := strings.Index(msg, "<vocabulary_reference>")
	bufferIdx := strings.Index(msg, "<input_buffer>")
	assert.True(t, vocabIdx < bufferIdx, "vocabulary_reference should appear before input_buffer")

	// task instruction at the end
	taskIdx := strings.Index(msg, "只输出音频中新听到的内容")
	assert.True(t, taskIdx > bufferIdx, "task instruction should appear after input_buffer")
}

func TestBuildUserMessage_Append_DoesNotContainEditInstructions(t *testing.T) {
	msg := buildUserMessage("append", "some text", "")
	assert.NotContains(t, msg, "编辑指令")
	assert.NotContains(t, msg, "删掉")
	assert.NotContains(t, msg, "改成")
}

// --- XML tag structure tests ---

func TestBuildUserMessage_VocabTag_ContainsOnlyData(t *testing.T) {
	chatCtx := "WuKongIM、唐僧叨叨"
	msg := buildUserMessage("edit", "", chatCtx)

	// Extract content between vocabulary_reference tags
	start := strings.Index(msg, "<vocabulary_reference>") + len("<vocabulary_reference>")
	end := strings.Index(msg, "</vocabulary_reference>")
	tagContent := strings.TrimSpace(msg[start:end])
	assert.Equal(t, chatCtx, tagContent, "tag should contain only the vocabulary data")
}

func TestBuildUserMessage_InputBufferTag_ContainsOnlyData(t *testing.T) {
	contextText := "Line 1\nLine 2\nLine 3"
	msg := buildUserMessage("edit", contextText, "")

	// Extract content between input_buffer tags
	start := strings.Index(msg, "<input_buffer>") + len("<input_buffer>")
	end := strings.Index(msg, "</input_buffer>")
	tagContent := strings.TrimSpace(msg[start:end])
	assert.Equal(t, contextText, tagContent, "tag should contain only the context data")
}

// --- Append vs Edit template difference ---

func TestBuildUserMessage_AppendWithVocab_UsesAppendTemplate(t *testing.T) {
	msg := buildUserMessage("append", "text", "vocab")
	assert.Contains(t, msg, "配合vocabulary_reference纠正专有名词拼写")
}

func TestBuildUserMessage_AppendNoVocab_UsesNoVocabTemplate(t *testing.T) {
	msg := buildUserMessage("append", "text", "")
	assert.NotContains(t, msg, "配合vocabulary_reference")
	assert.Contains(t, msg, "辅助你理解当前语境")
}

func TestBuildUserMessage_Edit_UsesEditTemplate(t *testing.T) {
	msg := buildUserMessage("edit", "text", "")
	assert.Contains(t, msg, "根据音频中的语音对其进行处理")
}

// --- IsNoSpeech tests ---

func TestIsNoSpeech(t *testing.T) {
	assert.True(t, IsNoSpeech(""))
	assert.True(t, IsNoSpeech("[NO_SPEECH]"))
	assert.True(t, IsNoSpeech("  [NO_SPEECH]  "))
	assert.True(t, IsNoSpeech("some prefix [NO_SPEECH]"))
	assert.False(t, IsNoSpeech("Hello world"))
	assert.False(t, IsNoSpeech("NO_SPEECH"))
}
