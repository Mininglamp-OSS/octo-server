package voice

import (
	"fmt"
	"strings"
)

const noSpeechSentinel = "[NO_SPEECH]"

const transcribePrompt = `你是一个严格的语音转写器，不是聊天助手，不是续写助手。

你的唯一任务：只根据音频中实际听到的人类语音生成转写结果。

严格规则（最高优先级）：
1. 如果音频中没有清晰可辨认的人类语音（静音、环境噪声、呼吸声、电流声、敲击声、模糊底噪），你必须且只能输出：
[NO_SPEECH]

2. 绝对禁止根据以下任一信息猜测、补全、联想、续写或生成内容：
   - 聊天上下文
   - 常识
   - 语义推断
   - 背景噪声

3. 只有在你明确听到可辨认的人类语音时，才允许输出转写文本。

4. 如果存在语音，准确还原说话内容，自动添加标点，修正明显口误和重复。

5. 输出只能是以下两种之一：
   - [NO_SPEECH]
   - 纯转写文本（不加任何解释）`

const modifyPromptTemplate = `你是一个严格的语音转写器和文本编辑器。

用户输入框中已有以下文本：
---
%s
---

严格规则（最高优先级）：
1. 如果音频中没有清晰可辨认的人类语音（静音、环境噪声、呼吸声、电流声），你必须且只能输出：
[NO_SPEECH]

2. 绝对禁止根据上下文、常识猜测或生成内容。

3. 如果听到编辑指令（如"删掉"、"删除"、"去掉"、"改成"、"替换"、"修改"），对已有文本执行相应操作。

4. 如果听到纯粹的新内容（不包含编辑指令），追加到已有文本末尾。

5. 输出只能是以下两种之一：
   - [NO_SPEECH]
   - 操作后的完整文本（不加任何解释）`

const chatContextPrefix = `以下聊天记录仅用于辅助识别专有名词拼写，不得作为转写内容来源：
---
%s
---
`

// buildPrompt returns the appropriate prompt based on whether context text and chat context are provided.
func buildPrompt(contextText string, chatContext string) string {
	var prompt string
	if contextText == "" {
		prompt = transcribePrompt
	} else {
		prompt = fmt.Sprintf(modifyPromptTemplate, contextText)
	}
	if chatContext != "" {
		prompt = fmt.Sprintf(chatContextPrefix, chatContext) + prompt
	}
	return prompt
}

// isNoSpeech checks if the model output indicates no speech was detected.
func isNoSpeech(text string) bool {
	if text == "" {
		return true
	}
	trimmed := strings.TrimSpace(text)
	return strings.Contains(trimmed, noSpeechSentinel)
}
