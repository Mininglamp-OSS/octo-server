package voice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- buildSystemMessage tests ---

func TestBuildSystemMessage_ReturnsSystemPrompt(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "智能语音转写器")
	assert.Contains(t, msg, "[NO_SPEECH]")
	assert.Contains(t, msg, "vocabulary_reference")
	assert.Contains(t, msg, "input_buffer")
	assert.Contains(t, msg, rule5TitleWithEmotion)
	assert.Contains(t, msg, emotionAnnotationSection)
	assert.NotContains(t, msg, "{{RULE5_TITLE}}")
	assert.NotContains(t, msg, "{{EMOTION_SECTION}}")
	assert.NotContains(t, msg, "{{EMOTION_EXAMPLES}}")
}

func TestBuildSystemMessage_ContainsAllRules(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "无语音判定")
	assert.Contains(t, msg, "禁止猜测")
	assert.Contains(t, msg, "编辑指令识别")
	assert.Contains(t, msg, "最小改写原则")
	assert.Contains(t, msg, "语言润色")
	assert.Contains(t, msg, "数据标签说明")
	assert.Contains(t, msg, "词汇参考表使用规则")
	assert.Contains(t, msg, "@提及识别")
	assert.Contains(t, msg, "输出格式")
}

func TestBuildSystemMessage_ContainsExamples(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "甲某")
	assert.Contains(t, msg, "乙某")
	assert.Contains(t, msg, "嗯，好的，我知道了")
}

func TestBuildSystemMessage_ContainsMentionRule(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "@提及识别")
	assert.Contains(t, msg, "艾特")
	assert.Contains(t, msg, "@Pythagoras")
	assert.Contains(t, msg, "@Boris")
	assert.Contains(t, msg, "让皮皮处理")
}

func TestBuildSystemMessage_MentionV2Scenarios(t *testing.T) {
	msg := buildSystemMessage(true)

	// Positive scenarios present in prompt
	assert.Contains(t, msg, "Boris，方案怎么样")       // direct address
	assert.Contains(t, msg, "跟Bob说明天开会改时间")    // preposition "跟"
	assert.Contains(t, msg, "这个方案怎么样，Boris")   // trailing address
	assert.Contains(t, msg, "让Boris不要动那个代码")   // stop action (still @)
	assert.Contains(t, msg, "跟宜林说一下")           // partial name

	// Negative scenarios present in prompt
	assert.Contains(t, msg, "Boris的代码写得不错")     // possessive, no @
	assert.Contains(t, msg, "告诉我Boris昨天说了什么") // asking third party, no @
	assert.Contains(t, msg, "Boris那边先不急")         // delay intent, no @

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
	assert.Contains(t, msg, "@Bob")
}

func TestBuildSystemMessage_MentionRuleOrder(t *testing.T) {
	msg := buildSystemMessage(true)
	vocabIdx := strings.Index(msg, "词汇参考表使用规则")
	mentionIdx := strings.Index(msg, "@提及识别")
	outputIdx := strings.Index(msg, "输出格式")
	assert.True(t, vocabIdx < mentionIdx, "@提及识别 should be after 词汇参考表使用规则")
	assert.True(t, mentionIdx < outputIdx, "@提及识别 should be before 输出格式")
}

func TestBuildUserMessage_WithMemberContext_MentionRuleAvailable(t *testing.T) {
	merged := BuildVocabularyReference("", "张三\n李四\nBob", "")
	userMsg := buildUserMessage("edit", "", merged, true)
	sysMsg := buildSystemMessage(true)

	assert.Contains(t, sysMsg, "@提及识别")
	assert.Contains(t, userMsg, "<member_vocabulary>")
	assert.Contains(t, userMsg, "张三")
}

// --- buildUserMessage: default/transcribe mode ---

func TestBuildUserMessage_Default_NoContext(t *testing.T) {
	msg := buildUserMessage("", "", "", true)
	assert.Equal(t, taskTranscribe, msg)
	assert.Contains(t, msg, "请转写音频中的语音")
	assert.NotContains(t, msg, "vocabulary_reference")
	assert.NotContains(t, msg, "input_buffer")
}

func TestBuildUserMessage_Default_WithVocab(t *testing.T) {
	msg := buildUserMessage("", "", "张三、李四", true)
	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "张三、李四")
	assert.Contains(t, msg, "不要输出纠错上下文中的任何内容")
	assert.NotContains(t, msg, "input_buffer")
}

// --- buildUserMessage: edit mode ---

func TestBuildUserMessage_Edit_NoContext(t *testing.T) {
	msg := buildUserMessage("edit", "", "", true)
	assert.Equal(t, taskTranscribe, msg)
}

func TestBuildUserMessage_Edit_WithContextText(t *testing.T) {
	msg := buildUserMessage("edit", "existing text", "", true)
	assert.Contains(t, msg, "<input_buffer>")
	assert.Contains(t, msg, "existing text")
	assert.Contains(t, msg, "根据音频中的语音对其进行处理")
	assert.Contains(t, msg, "编辑指令")
	assert.NotContains(t, msg, "vocabulary_reference")
}

func TestBuildUserMessage_Edit_WithVocabOnly(t *testing.T) {
	msg := buildUserMessage("edit", "", "Alice: 测试", true)
	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "Alice: 测试")
	assert.Contains(t, msg, "不要输出纠错上下文中的任何内容")
	assert.NotContains(t, msg, "input_buffer")
}

func TestBuildUserMessage_Edit_WithBothContexts(t *testing.T) {
	msg := buildUserMessage("edit", "existing draft", "Alice: 专有名词ABC", true)

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
	msg := buildUserMessage("edit_only", "", "", true)
	assert.Equal(t, taskTranscribe, msg)
}

func TestBuildUserMessage_EditOnly_WithContextText(t *testing.T) {
	msg := buildUserMessage("edit_only", "existing text", "", true)
	assert.Contains(t, msg, "<input_buffer>")
	assert.Contains(t, msg, "existing text")
	assert.Contains(t, msg, "根据音频中的语音对其进行处理")
	assert.Contains(t, msg, "编辑上述文本")
	assert.Contains(t, msg, "原样返回已有文本")
	assert.NotContains(t, msg, "vocabulary_reference")
}

func TestBuildUserMessage_EditOnly_WithVocabOnly(t *testing.T) {
	msg := buildUserMessage("edit_only", "", "Alice: 测试", true)
	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "Alice: 测试")
	assert.Contains(t, msg, "不要输出纠错上下文中的任何内容")
	assert.NotContains(t, msg, "input_buffer")
}

func TestBuildUserMessage_EditOnly_WithBothContexts(t *testing.T) {
	msg := buildUserMessage("edit_only", "existing draft", "Alice: 专有名词ABC", true)

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
	msg := buildUserMessage("edit_only", "some text", "", true)
	assert.NotContains(t, msg, "追加到已有文本末尾")
	assert.Contains(t, msg, "原样返回已有文本，不要追加任何内容")
}

// --- buildUserMessage: append mode ---

func TestBuildUserMessage_Append_NoContext(t *testing.T) {
	msg := buildUserMessage("append", "", "", true)
	assert.Equal(t, taskTranscribe, msg)
}

func TestBuildUserMessage_Append_WithContextText_NoVocab(t *testing.T) {
	msg := buildUserMessage("append", "已有的文本内容", "", true)
	assert.Contains(t, msg, "<input_buffer>")
	assert.Contains(t, msg, "已有的文本内容")
	assert.Contains(t, msg, "辅助你理解当前语境")
	assert.Contains(t, msg, "只输出音频中新听到的内容")
	assert.NotContains(t, msg, "vocabulary_reference")
	assert.NotContains(t, msg, "编辑指令")
}

func TestBuildUserMessage_Append_WithVocabOnly(t *testing.T) {
	msg := buildUserMessage("append", "", "Alice: 聊天内容", true)
	assert.Contains(t, msg, "<vocabulary_reference>")
	assert.Contains(t, msg, "Alice: 聊天内容")
	assert.NotContains(t, msg, "input_buffer")
}

func TestBuildUserMessage_Append_WithBothContexts(t *testing.T) {
	msg := buildUserMessage("append", "原有文本", "Alice: 聊天", true)

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
	msg := buildUserMessage("append", "some text", "", true)
	assert.NotContains(t, msg, "删掉")
	assert.NotContains(t, msg, "改成")
	assert.NotContains(t, msg, "编辑指令")
}

// --- XML tag structure tests ---

func TestBuildUserMessage_VocabTag_ContainsOnlyData(t *testing.T) {
	chatCtx := "WuKongIM、唐僧叨叨"
	msg := buildUserMessage("edit", "", chatCtx, true)

	// Extract content between vocabulary_reference tags
	start := strings.Index(msg, "<vocabulary_reference>") + len("<vocabulary_reference>")
	end := strings.Index(msg, "</vocabulary_reference>")
	tagContent := strings.TrimSpace(msg[start:end])
	assert.Equal(t, chatCtx, tagContent, "tag should contain only the vocabulary data")
}

func TestBuildUserMessage_InputBufferTag_ContainsOnlyData(t *testing.T) {
	contextText := "Line 1\nLine 2\nLine 3"
	msg := buildUserMessage("edit", contextText, "", true)

	// Extract content between input_buffer tags
	start := strings.Index(msg, "<input_buffer>") + len("<input_buffer>")
	end := strings.Index(msg, "</input_buffer>")
	tagContent := strings.TrimSpace(msg[start:end])
	assert.Equal(t, contextText, tagContent, "tag should contain only the context data")
}

// --- Append vs Edit template difference ---

func TestBuildUserMessage_AppendWithVocab_UsesAppendTemplate(t *testing.T) {
	msg := buildUserMessage("append", "text", "vocab", true)
	assert.Contains(t, msg, "配合vocabulary_reference纠正专有名词拼写")
}

func TestBuildUserMessage_AppendNoVocab_UsesNoVocabTemplate(t *testing.T) {
	msg := buildUserMessage("append", "text", "", true)
	assert.NotContains(t, msg, "配合vocabulary_reference")
	assert.Contains(t, msg, "辅助你理解当前语境")
}

func TestBuildUserMessage_Edit_UsesEditTemplate(t *testing.T) {
	msg := buildUserMessage("edit", "text", "", true)
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

// --- Emotion annotation toggle tests ---

func TestBuildSystemMessage_EmotionEmojiEnabled(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "5.3 情绪标注")
	assert.Contains(t, msg, "😄")
	assert.Contains(t, msg, "[有品位]")
	assert.Contains(t, msg, "[崇尚行动]")
	assert.Contains(t, msg, "[使命必达]")
}

func TestBuildSystemMessage_EmotionEmojiDisabled(t *testing.T) {
	msg := buildSystemMessage(false)
	assert.NotContains(t, msg, "5.3 情绪标注")
	assert.NotContains(t, msg, "忽略情绪标注视为转写失败")
	assert.Contains(t, msg, "5.1 标点必须匹配语音语气")
	assert.Contains(t, msg, "5.2 句尾语气助词保留")
	assert.Contains(t, msg, "规则 5：语气保真")
	assert.NotContains(t, msg, "规则 5：语气保真与情绪标注")
	assert.NotContains(t, msg, "😄")
	assert.NotContains(t, msg, "😮‍💨")
	assert.NotContains(t, msg, "[崇尚行动]")
	assert.NotContains(t, msg, "[使命必达]")
	assert.NotContains(t, msg, "[有品位]")
}

func TestBuildSystemMessage_EmotionDisabled_CustomPrompt(t *testing.T) {
	t.Cleanup(resetToDefaults)

	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.yaml")
	customSystem := "Custom prefix.\n\nCustom suffix."
	content := "system: |\n  " + strings.ReplaceAll(customSystem, "\n", "\n  ") + "\n"
	os.WriteFile(path, []byte(content), 0644)

	LoadPrompts(path, nil)

	msg := buildSystemMessage(false)
	assert.Equal(t, "Custom prefix.\n\nCustom suffix.", msg)
	assert.Contains(t, msg, "Custom prefix.")
	assert.Contains(t, msg, "Custom suffix.")
}

func TestBuildSystemMessage_EmotionDisabled_NoEmotionSelfCheck(t *testing.T) {
	msg := buildSystemMessage(false)
	assert.NotContains(t, msg, "情绪检查")
	assert.NotContains(t, msg, "情绪标注自查")
	assert.NotContains(t, msg, "音频是否有明显情绪")
	// Non-emotion self-check items must still be present
	assert.Contains(t, msg, "书面化检查")
	assert.Contains(t, msg, "逐字忠实检查")
	assert.Contains(t, msg, "模式检查")
}

// --- New rule 5 / examples tests ---

func TestBuildSystemMessage_ContainsRule5(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "规则 5：语气保真与情绪标注")
	assert.Contains(t, msg, "5.1 标点必须匹配语音语气")
	assert.Contains(t, msg, "5.2 句尾语气助词保留")
	assert.Contains(t, msg, "5.3 情绪标注")
}

func TestBuildSystemMessage_ContainsNewExamples(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "示例7")
	assert.Contains(t, msg, "示例12")
	assert.Contains(t, msg, "示例15")
	assert.Contains(t, msg, "示例17")
	assert.Contains(t, msg, "赞赏品味")
}

func TestBuildSystemMessage_OutputSelfCheck(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "书面化检查")
	assert.Contains(t, msg, "逐字忠实检查")
	assert.Contains(t, msg, "模式检查")
	assert.Contains(t, msg, "情绪标注自查")
}

func TestBuildSystemMessage_EmotionDisabled_NoEmotionSection(t *testing.T) {
	msg := buildSystemMessage(false)
	assert.NotContains(t, msg, emotionAnnotationSection)
	assert.NotContains(t, msg, "5.3 情绪标注")
	assert.NotContains(t, msg, "情绪标注自查")
}

func TestBuildSystemMessage_EmotionDisabled_Rule5Title(t *testing.T) {
	msg := buildSystemMessage(false)
	assert.Contains(t, msg, "规则 5：语气保真")
	assert.NotContains(t, msg, "规则 5：语气保真与情绪标注")
}

func TestTaskEdit_ContainsEmotionReference(t *testing.T) {
	assert.Contains(t, taskEdit, "包括语气保真与情绪标注")
}

func TestTaskEditOnly_ContainsEmotionReference(t *testing.T) {
	assert.Contains(t, taskEditOnly, "包括语气保真与情绪标注")
}

// --- Space preservation in @mention ---

func TestBuildSystemMessage_SpacePreservationRule(t *testing.T) {
	msg := buildSystemMessage(true)

	// The prompt must contain the space-preservation rule
	assert.Contains(t, msg, "空格保真铁律")
	assert.Contains(t, msg, "有空格就保留空格，没有空格绝对不能添加空格")

	// EXAMPLE demonstrates both with-space and without-space cases
	assert.Contains(t, msg, "tomas.fu (托马斯.福)")
	assert.Contains(t, msg, "王磊(Rock)")
}

func TestBuildSystemMessage_MentionExampleWithParenNoSpace(t *testing.T) {
	msg := buildSystemMessage(true)

	// The example member list must include a no-space paren name
	assert.Contains(t, msg, "王磊(Rock),Bob")

	// The example output must show correct no-space format
	assert.Contains(t, msg, `@王磊(Rock) 看一下这个 bug`)
	assert.Contains(t, msg, "列表中无空格，输出不加空格")
}

func TestBuildUserMessage_ParenNameInMemberVocabulary(t *testing.T) {
	// Simulate the real scenario: member list has "王磊(Rock)" (no space)
	// and "tomas.fu (托马斯.福)" (with space), no latest_chat_context for 王磊
	memberCtx := "聊天成员：王磊(Rock),tomas.fu (托马斯.福),张三"
	chatCtx := "[张三]: 大家好\n[tomas.fu (托马斯.福)]: 你好"
	merged := BuildVocabularyReference("", memberCtx, chatCtx)
	userMsg := buildUserMessage("", "", merged, true)

	// The vocabulary reference must preserve the exact spacing from member list
	assert.Contains(t, userMsg, "王磊(Rock)")
	assert.Contains(t, userMsg, "tomas.fu (托马斯.福)")

	// System prompt must have the rule active
	sysMsg := buildSystemMessage(true)
	assert.Contains(t, sysMsg, "原样输出")
}

func TestBuildSystemMessage_PartialNameRule(t *testing.T) {
	msg := buildSystemMessage(true)

	// Rule 3 (partial match) must emphasize verbatim copy from member list
	assert.Contains(t, msg, "原样输出该成员在列表中的完整条目")
}

// --- v5 + example data isolation tests ---

func TestSystemPromptContainsExampleIsolationDeclaration(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "⚠️ 重要声明（最高优先级）")
	assert.Contains(t, msg, "<EXAMPLE>")
	assert.Contains(t, msg, "绝对禁止将 <EXAMPLE> 内的任何人名、映射关系应用到实际转写输出中")
}

func TestSystemPromptExampleSectionsAreWrapped(t *testing.T) {
	msg := buildSystemMessage(true)

	// Count structural EXAMPLE tags (on their own line) vs inline mentions
	openCount := strings.Count(msg, "\n<EXAMPLE>\n")
	closeCount := strings.Count(msg, "\n</EXAMPLE>\n")
	// Handle potential tag at end of string without trailing newline
	if strings.HasSuffix(msg, "\n</EXAMPLE>") {
		closeCount++
	}
	assert.GreaterOrEqual(t, openCount, 4, "at least 4 EXAMPLE sections expected")
	assert.Equal(t, openCount, closeCount, "EXAMPLE open/close tags must be balanced")

	// Verify tags alternate correctly: every <EXAMPLE> is closed before the next opens
	remaining := msg
	for i := 0; i < openCount; i++ {
		openIdx := strings.Index(remaining, "\n<EXAMPLE>\n")
		assert.True(t, openIdx >= 0, "expected <EXAMPLE> tag #%d", i+1)
		remaining = remaining[openIdx+len("\n<EXAMPLE>\n"):]
		closeIdx := strings.Index(remaining, "\n</EXAMPLE>")
		nextOpen := strings.Index(remaining, "\n<EXAMPLE>\n")
		assert.True(t, closeIdx >= 0, "expected </EXAMPLE> for tag #%d", i+1)
		if nextOpen >= 0 {
			assert.True(t, closeIdx < nextOpen,
				"</EXAMPLE> must come before next <EXAMPLE> (tag #%d)", i+1)
		}
		remaining = remaining[closeIdx+len("\n</EXAMPLE>"):]
	}

	// Verify key example content is inside <EXAMPLE>...</EXAMPLE> blocks
	exampleContents := []string{
		"请把销售额从120",
		"嗯甲某",
		"甲某负责前端",
		"@Pythagoras 看一下",
	}
	for _, content := range exampleContents {
		idx := strings.Index(msg, content)
		assert.True(t, idx >= 0, "content %q must exist in prompt", content)
		before := msg[:idx]
		after := msg[idx:]
		lastOpen := strings.LastIndex(before, "\n<EXAMPLE>\n")
		lastClose := strings.LastIndex(before, "\n</EXAMPLE>")
		nextClose := strings.Index(after, "\n</EXAMPLE>")
		assert.True(t, lastOpen > lastClose,
			"content %q must be after an unclosed <EXAMPLE>", content)
		assert.True(t, nextClose >= 0,
			"content %q must have a closing </EXAMPLE> after it", content)
	}

	// Verify no example-style keywords appear outside <EXAMPLE> blocks
	exampleKeywords := []string{"示例1", "示例2", "正确输出：", "❌ 错误输出：", "❌ 错误：", "❌ 禁止："}
	outsideText := extractTextOutsideExampleTags(msg)
	for _, kw := range exampleKeywords {
		assert.NotContains(t, outsideText, kw,
			"example keyword %q must not appear outside <EXAMPLE> blocks", kw)
	}
}

func extractTextOutsideExampleTags(text string) string {
	var outside strings.Builder
	remaining := text
	for {
		openIdx := strings.Index(remaining, "\n<EXAMPLE>\n")
		if openIdx < 0 {
			outside.WriteString(remaining)
			break
		}
		outside.WriteString(remaining[:openIdx])
		remaining = remaining[openIdx+len("\n<EXAMPLE>\n"):]
		closeIdx := strings.Index(remaining, "\n</EXAMPLE>")
		if closeIdx < 0 {
			break
		}
		remaining = remaining[closeIdx+len("\n</EXAMPLE>"):]
	}
	return outside.String()
}

func TestSystemPromptContainsEditInstructionRecognition(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "编辑指令识别")
	assert.Contains(t, msg, "追加新内容")
	assert.Contains(t, msg, "在 edit 模式下")
}

func TestAppendMode_NoEditModeTrigger(t *testing.T) {
	sysMsg := buildSystemMessage(true)
	assert.NotContains(t, sysMsg, "当用户消息中包含 <input_buffer> 时，你处于")
	assert.Contains(t, sysMsg, "在 edit 模式下")
}

func TestEditOnlyMode_NoAppendFallback(t *testing.T) {
	sysMsg := buildSystemMessage(true)
	assert.NotContains(t, sysMsg, "将语音转写内容追加到 ORIGINAL 末尾")
	assert.NotContains(t, sysMsg, "你必须输出完整文本，包含 ORIGINAL 部分")
}

func TestSystemPromptContainsMinimalRewriteRule(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "最小改写原则（逐字忠实）")
	assert.Contains(t, msg, "严格保持原句序")
	assert.Contains(t, msg, "不得调换语序")
}

func TestSystemPromptContainsContentWordProtection(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "请把")
	assert.Contains(t, msg, "麻烦")
	assert.Contains(t, msg, "实义词")
	assert.Contains(t, msg, "礼貌用语是实义内容，不是填充词")
}

func TestSystemPromptContainsArabicNumeralRule(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "阿拉伯数字")
	assert.Contains(t, msg, "→ 120")
	assert.Contains(t, msg, "→ 20%")
}

func TestSystemPromptContainsEmotionMandatoryDeclaration(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "忽略情绪标注视为转写失败")
	assert.Contains(t, msg, "必须执行")
}

func TestSystemPromptFillerWordZeroTolerance(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "零容忍")
	assert.Contains(t, msg, "必须彻底删除")
	assert.Contains(t, msg, "句首")
	assert.Contains(t, msg, "处理规则")
	assert.Contains(t, msg, "保留（表示肯定）")
	assert.Contains(t, msg, "保留（表示疑问）")
}

// --- buildUserMessage: emotion disabled regression tests ---

func TestBuildUserMessage_Edit_EmotionDisabled_NoEmotionInTask(t *testing.T) {
	msg := buildUserMessage("edit", "existing text", "", false)
	assert.Contains(t, msg, "（包括语气保真）")
	assert.NotContains(t, msg, "情绪标注")
}

func TestBuildUserMessage_EditOnly_EmotionDisabled_NoEmotionInTask(t *testing.T) {
	msg := buildUserMessage("edit_only", "existing text", "", false)
	assert.Contains(t, msg, "（包括语气保真）")
	assert.NotContains(t, msg, "情绪标注")
}

func TestBuildUserMessage_Append_EmotionDisabled_NoEmotionInTask(t *testing.T) {
	msg := buildUserMessage("append", "已有文本", "", false)
	assert.Contains(t, msg, "（包括语气保真）")
	assert.NotContains(t, msg, "情绪标注")
}

func TestBuildUserMessage_Append_EmotionEnabled_HasEmotionInTask(t *testing.T) {
	msg := buildUserMessage("append", "已有文本", "", true)
	assert.Contains(t, msg, "情绪标注")
}

func TestBuildSystemMessage_PercentSafe(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "→ 20%")
	assert.NotContains(t, msg, "%!")
}

func TestBuildSystemMessage_EmotionEnabled_HasEmotionExamples(t *testing.T) {
	msg := buildSystemMessage(true)
	assert.Contains(t, msg, "😄")
	assert.Contains(t, msg, "😮‍💨")
	assert.Contains(t, msg, "[崇尚行动]")
	assert.Contains(t, msg, "[使命必达]")
	assert.Contains(t, msg, "[有品位]")
}

func TestBuildSystemMessage_EmotionDisabled_NoEmotionExamples(t *testing.T) {
	msg := buildSystemMessage(false)
	assert.NotContains(t, msg, "😄")
	assert.NotContains(t, msg, "😮‍💨")
	assert.NotContains(t, msg, "[崇尚行动]")
	assert.NotContains(t, msg, "[使命必达]")
	assert.NotContains(t, msg, "[有品位]")
}
