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
	assert.Contains(t, msg, "@提及识别")
	assert.Contains(t, msg, "输出格式")
}

func TestBuildSystemMessage_ContainsExamples(t *testing.T) {
	msg := buildSystemMessage()
	assert.Contains(t, msg, "大背头")
	assert.Contains(t, msg, "托马斯")
	assert.Contains(t, msg, "嗯，好的，我知道了")
}

func TestBuildSystemMessage_ContainsMentionRule(t *testing.T) {
	msg := buildSystemMessage()
	assert.Contains(t, msg, "@提及识别")
	assert.Contains(t, msg, "艾特")
	assert.Contains(t, msg, "@Pythagoras")
	assert.Contains(t, msg, "@Bob")
}

func TestBuildSystemMessage_MentionV2Scenarios(t *testing.T) {
	msg := buildSystemMessage()

	// Positive scenarios present in prompt
	assert.Contains(t, msg, "让皮皮处理")            // intent verb "让"
	assert.Contains(t, msg, "Boris，方案怎么样")      // direct address
	assert.Contains(t, msg, "跟Bob说明天开会改时间")   // preposition "跟"
	assert.Contains(t, msg, "这个方案怎么样，Boris")  // trailing address
	assert.Contains(t, msg, "让Boris不要动那个代码")  // stop action (still @)
	assert.Contains(t, msg, "跟宜林说一下")          // partial name

	// Negative scenarios present in prompt
	assert.Contains(t, msg, "Boris的代码写得不错")    // possessive, no @
	assert.Contains(t, msg, "告诉我Boris昨天说了什么") // asking third party, no @
	assert.Contains(t, msg, "Boris那边先不急")        // delay intent, no @

	// Structural elements
	assert.Contains(t, msg, "通用判断原则")
	assert.Contains(t, msg, "召回优先")
	assert.Contains(t, msg, "人名匹配规则")
	assert.Contains(t, msg, "翻译名/别名匹配")
	assert.Contains(t, msg, "部分名/简称匹配")
	assert.Contains(t, msg, "常见触发模式")
	assert.Contains(t, msg, "直接对话")
	assert.Contains(t, msg, "沟通介词")
	assert.Contains(t, msg, "句尾呼唤")

	// Disambiguation line
	assert.Contains(t, msg, "暂不联系/延迟处理")
}

func TestBuildSystemMessage_MentionRuleOrder(t *testing.T) {
	msg := buildSystemMessage()
	vocabIdx := strings.Index(msg, "词汇参考表使用规则")
	mentionIdx := strings.Index(msg, "@提及识别")
	outputIdx := strings.Index(msg, "输出格式")
	assert.True(t, vocabIdx < mentionIdx, "@提及识别 should be after 词汇参考表使用规则")
	assert.True(t, mentionIdx < outputIdx, "@提及识别 should be before 输出格式")
}

func TestBuildUserMessage_WithMemberContext_MentionRuleAvailable(t *testing.T) {
	merged := BuildVocabularyReference("", "张三\n李四\nBob", "")
	userMsg := buildUserMessage("edit", "", merged)
	sysMsg := buildSystemMessage()

	assert.Contains(t, sysMsg, "@提及识别")
	assert.Contains(t, userMsg, "<member_vocabulary>")
	assert.Contains(t, userMsg, "张三")
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

// --- buildUserMessage: edit_only mode ---

func TestBuildUserMessage_EditOnly_NoContext(t *testing.T) {
	msg := buildUserMessage("edit_only", "", "")
	assert.Equal(t, taskTranscribe, msg)
}

func TestBuildUserMessage_EditOnly_WithContextText(t *testing.T) {
	msg := buildUserMessage("edit_only", "existing text", "")
	assert.Contains(t, msg, "<input_buffer>")
	assert.Contains(t, msg, "existing text")
	assert.Contains(t, msg, "根据音频中的语音对其进行处理")
	assert.Contains(t, msg, "编辑上述文本")
	assert.Contains(t, msg, "原样返回已有文本")
	assert.NotContains(t, msg, "vocabulary_reference")
}

func TestBuildUserMessage_EditOnly_WithVocabOnly(t *testing.T) {
	msg := buildUserMessage("edit_only", "", "Alice: 测试")
	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "Alice: 测试")
	assert.Contains(t, msg, "不要输出纠错上下文中的任何内容")
	assert.NotContains(t, msg, "input_buffer")
}

func TestBuildUserMessage_EditOnly_WithBothContexts(t *testing.T) {
	msg := buildUserMessage("edit_only", "existing draft", "Alice: 专有名词ABC")

	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "Alice: 专有名词ABC")
	assert.Contains(t, msg, "<input_buffer>")
	assert.Contains(t, msg, "existing draft")
	assert.Contains(t, msg, "原样返回已有文本")

	// vocabulary_reference should appear before input_buffer
	vocabIdx := strings.Index(msg, "<vocabulary_reference>")
	bufferIdx := strings.Index(msg, "<input_buffer>")
	assert.True(t, vocabIdx < bufferIdx, "vocabulary_reference should appear before input_buffer")

	// task instruction at the end
	taskIdx := strings.Index(msg, "编辑上述文本")
	assert.True(t, taskIdx > bufferIdx, "task instruction should appear after input_buffer")
}

func TestBuildUserMessage_EditOnly_DoesNotContainAppendFallback(t *testing.T) {
	msg := buildUserMessage("edit_only", "some text", "")
	assert.NotContains(t, msg, "追加到已有文本末尾")
	assert.Contains(t, msg, "原样返回已有文本，不要追加任何内容")
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

// --- BuildVocabularyReference tests ---

func TestBuildVocabularyReference_AllEmpty(t *testing.T) {
	result := BuildVocabularyReference("", "", "")
	assert.Equal(t, "", result)
}

func TestBuildVocabularyReference_OnlyChatContext(t *testing.T) {
	result := BuildVocabularyReference("", "", "chat messages here")
	assert.Equal(t, "chat messages here", result)
}

func TestBuildVocabularyReference_OnlyPersonal(t *testing.T) {
	result := BuildVocabularyReference("my terms", "", "")
	assert.Contains(t, result, "<personal_vocabulary>")
	assert.Contains(t, result, "my terms")
	assert.NotContains(t, result, "<member_vocabulary>")
	assert.NotContains(t, result, "<latest_chat_context>")
}

func TestBuildVocabularyReference_OnlyMember(t *testing.T) {
	result := BuildVocabularyReference("", "Alice, Bob", "")
	assert.Contains(t, result, "<member_vocabulary>")
	assert.Contains(t, result, "Alice, Bob")
	assert.NotContains(t, result, "<personal_vocabulary>")
	assert.NotContains(t, result, "<latest_chat_context>")
}

func TestBuildVocabularyReference_PersonalAndMember(t *testing.T) {
	result := BuildVocabularyReference("my terms", "Alice, Bob", "")
	assert.Contains(t, result, "<personal_vocabulary>")
	assert.Contains(t, result, "my terms")
	assert.Contains(t, result, "<member_vocabulary>")
	assert.Contains(t, result, "Alice, Bob")
	assert.NotContains(t, result, "<latest_chat_context>")

	pIdx := strings.Index(result, "<personal_vocabulary>")
	mIdx := strings.Index(result, "<member_vocabulary>")
	assert.True(t, pIdx < mIdx, "personal should appear before member")
}

func TestBuildVocabularyReference_PersonalAndChat(t *testing.T) {
	result := BuildVocabularyReference("my terms", "", "chat messages")
	assert.Contains(t, result, "<personal_vocabulary>")
	assert.Contains(t, result, "my terms")
	assert.Contains(t, result, "<latest_chat_context>")
	assert.Contains(t, result, "chat messages")
	assert.NotContains(t, result, "<member_vocabulary>")

	pIdx := strings.Index(result, "<personal_vocabulary>")
	cIdx := strings.Index(result, "<latest_chat_context>")
	assert.True(t, pIdx < cIdx, "personal should appear before chat")
}

func TestBuildVocabularyReference_AllThree(t *testing.T) {
	result := BuildVocabularyReference("my terms", "Alice, Bob", "chat messages")
	assert.Contains(t, result, "<personal_vocabulary>")
	assert.Contains(t, result, "my terms")
	assert.Contains(t, result, "<member_vocabulary>")
	assert.Contains(t, result, "Alice, Bob")
	assert.Contains(t, result, "<latest_chat_context>")
	assert.Contains(t, result, "chat messages")

	pIdx := strings.Index(result, "<personal_vocabulary>")
	mIdx := strings.Index(result, "<member_vocabulary>")
	cIdx := strings.Index(result, "<latest_chat_context>")
	assert.True(t, pIdx < mIdx, "personal should appear before member")
	assert.True(t, mIdx < cIdx, "member should appear before chat")
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
