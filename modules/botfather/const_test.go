package botfather

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
)

func TestBotFatherConstants(t *testing.T) {
	assert.Equal(t, "botfather", BotFatherUID)
	assert.Equal(t, "BotFather", BotFatherName)
	assert.Equal(t, "bf_", BotTokenPrefix)
	assert.Equal(t, "_bot", BotUsernameSuffix)
}

func TestCommandConstants(t *testing.T) {
	commands := []struct {
		name string
		cmd  string
	}{
		{"newbot", CmdNewBot},
		{"mybots", CmdMyBots},
		{"connect", CmdConnect},
		{"disconnect", CmdDisconnect},
		{"setname", CmdSetName},
		{"setdescription", CmdSetDescription},
		{"deletebot", CmdDeleteBot},
		{"token", CmdToken},
		{"revoke", CmdRevoke},
		{"cancel", CmdCancel},
		{"help", CmdHelp},
		{"start", CmdStart},
	}

	for _, tt := range commands {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, strings.HasPrefix(tt.cmd, "/"),
				"command %s should start with /", tt.cmd)
			assert.NotEmpty(t, tt.cmd)
		})
	}

	// 验证所有命令唯一
	allCmds := []string{
		CmdNewBot, CmdMyBots, CmdConnect, CmdDisconnect,
		CmdSetName, CmdSetDescription, CmdDeleteBot,
		CmdToken, CmdRevoke, CmdCancel, CmdHelp, CmdStart,
	}
	seen := make(map[string]bool)
	for _, cmd := range allCmds {
		assert.False(t, seen[cmd], "command %s is duplicated", cmd)
		seen[cmd] = true
	}
}

func TestStateConstants(t *testing.T) {
	assert.Equal(t, "", StateNone)
	assert.Equal(t, "waiting_bot_name", StateWaitingBotName)
	assert.Equal(t, "waiting_bot_username", StateWaitingBotUsername)
	assert.Equal(t, "waiting_select_bot", StateWaitingSelectBot)
	assert.Equal(t, "waiting_new_name", StateWaitingNewName)
	assert.Equal(t, "waiting_description", StateWaitingDescription)
	assert.Equal(t, "waiting_delete_confirm", StateWaitingDeleteConfirm)
	assert.Equal(t, "waiting_revoke_confirm", StateWaitingRevokeConfirm)
}

func TestFieldConstants(t *testing.T) {
	assert.Equal(t, "state", FieldState)
	assert.Equal(t, "command", FieldCommand)
	assert.Equal(t, "bot_id", FieldBotID)
	assert.Equal(t, "bot_name", FieldBotName)
}

func TestGenerateBotToken(t *testing.T) {
	token, err := generateBotToken()
	assert.NoError(t, err)

	// 应以 BotTokenPrefix 开头
	assert.True(t, strings.HasPrefix(token, BotTokenPrefix),
		"token should start with %s, got %s", BotTokenPrefix, token)

	// 去掉前缀后应该是 32 个16进制字符（16 bytes = 32 hex chars）
	hex := strings.TrimPrefix(token, BotTokenPrefix)
	assert.Len(t, hex, 32, "hex part should be 32 chars")

	// 验证是合法的16进制字符
	for _, c := range hex {
		assert.True(t,
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
			"invalid hex char: %c", c)
	}
}

func TestGenerateBotToken_Uniqueness(t *testing.T) {
	tokens := make(map[string]bool)
	for i := 0; i < 100; i++ {
		token, err := generateBotToken()
		assert.NoError(t, err)
		assert.False(t, tokens[token], "duplicate token generated")
		tokens[token] = true
	}
}

func TestRandomHex(t *testing.T) {
	tests := []struct {
		name     string
		n        int
		wantLen  int
	}{
		{"1 byte", 1, 2},
		{"4 bytes", 4, 8},
		{"8 bytes", 8, 16},
		{"16 bytes", 16, 32},
		{"32 bytes", 32, 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hex, err := randomHex(tt.n)
			assert.NoError(t, err)
			assert.Len(t, hex, tt.wantLen, "randomHex(%d) should return %d chars", tt.n, tt.wantLen)

			// 所有字符应为合法16进制
			for _, c := range hex {
				assert.True(t,
					(c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
					"invalid hex char: %c", c)
			}
		})
	}
}

func TestGenerateSkillMD(t *testing.T) {
	apiURL := "http://localhost:8090"
	wsURL := "ws://localhost:5200"

	md := generateSkillMD(apiURL, wsURL)

	// 应包含关键内容
	assert.Contains(t, md, "dmwork")
	assert.Contains(t, md, apiURL)
	assert.Contains(t, md, wsURL)
	assert.Contains(t, md, "/v1/bot/register")
	assert.Contains(t, md, "/v1/bot/sendMessage")
	assert.Contains(t, md, "/v1/bot/events")
	assert.Contains(t, md, "/v1/bot/heartbeat")
	assert.Contains(t, md, "/v1/bot/typing")
	assert.Contains(t, md, "Authorization: Bearer")
}

func TestDeriveWSURL(t *testing.T) {
	tests := []struct {
		name       string
		apiURL     string
		externalIP string
		want       string
	}{
		{
			name:       "standard http",
			apiURL:     "http://127.0.0.1:5001",
			externalIP: "",
			want:       "ws://127.0.0.1:5200",
		},
		{
			name:       "https",
			apiURL:     "https://example.com:5001",
			externalIP: "",
			want:       "ws://example.com:5200",
		},
		{
			name:       "with external IP",
			apiURL:     "http://127.0.0.1:5001",
			externalIP: "1.2.3.4",
			want:       "ws://1.2.3.4:5200",
		},
		{
			name:       "no port in API URL",
			apiURL:     "http://myhost",
			externalIP: "",
			want:       "ws://myhost:5200",
		},
		{
			name:       "external IP overrides host",
			apiURL:     "http://internal:5001",
			externalIP: "external.example.com",
			want:       "ws://external.example.com:5200",
		},
		{
			name:       "whitespace external IP ignored",
			apiURL:     "http://192.168.1.1:5001",
			externalIP: "  ",
			want:       "ws://192.168.1.1:5200",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.WuKongIM.APIURL = tt.apiURL
			cfg.External.IP = tt.externalIP
			got := deriveWSURL(cfg)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRedisKeyConstants(t *testing.T) {
	assert.Equal(t, "botfather:state:", stateKeyPrefix)
	assert.Equal(t, 600, stateTTL)
	assert.Equal(t, "bot:heartbeat:", heartbeatKeyPrefix)
	assert.Equal(t, 60, heartbeatTTL)
}

func TestStateMachineKey(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	sm := newStateMachine(ctx)

	// 测试 key 生成
	key := sm.key("user_001", "")
	assert.Equal(t, "botfather:state:user_001", key)

	key2 := sm.key("user_002", "")
	assert.Equal(t, "botfather:state:user_002", key2)

	// 不同 UID 应产生不同 key
	assert.NotEqual(t, key, key2)
}

func TestStateMachineKey_EmptyUID(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	sm := newStateMachine(ctx)

	key := sm.key("", "")
	assert.Equal(t, "botfather:state:", key)
}

func TestBotUsernameValidation(t *testing.T) {
	// 模拟 onBotUsernameInput 中的验证逻辑
	tests := []struct {
		name    string
		input   string
		valid   bool
	}{
		{"valid alphanumeric", "mybot123", true},
		{"valid with underscore", "my_bot", true},
		{"valid single char", "a", true},
		{"valid max length", strings.Repeat("a", 20), true},
		{"too long", strings.Repeat("a", 21), false},
		{"empty", "", false},
		{"uppercase converted", "MyBot", true}, // 转小写后验证
		{"with hyphen", "my-bot", false},
		{"with space", "my bot", false},
		{"with dot", "my.bot", false},
		{"with chinese", "机器人", false},
		{"only underscores", "___", true},
		{"only numbers", "12345", true},
		{"starts with number", "1bot", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.TrimSpace(tt.input)
			input = strings.ToLower(input)
			input = strings.TrimSuffix(input, BotUsernameSuffix)

			valid := true
			if len(input) == 0 || len(input) > 20 {
				valid = false
			} else {
				for _, r := range input {
					if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
						valid = false
						break
					}
				}
			}
			assert.Equal(t, tt.valid, valid, "input: %q", tt.input)
		})
	}
}

func TestBotNameValidation(t *testing.T) {
	// 模拟 onBotNameInput 中的验证逻辑
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"valid short name", "Bot", true},
		{"valid long name", strings.Repeat("名", 64), true},
		{"too long", strings.Repeat("a", 65), false},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"single char", "A", true},
		{"exactly 64 chars", strings.Repeat("x", 64), true},
		{"with spaces", "My Cool Bot", true},
		{"unicode name", "机器人🤖", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := strings.TrimSpace(tt.input)
			valid := len(name) > 0 && len(name) <= 64
			assert.Equal(t, tt.valid, valid, "input: %q", tt.input)
		})
	}
}

func TestBotTokenPrefix_Format(t *testing.T) {
	// Token 以 bf_ 开头
	token, err := generateBotToken()
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(token, "bf_"))

	// Token 长度应为 bf_ (3) + 32 hex chars = 35
	assert.Len(t, token, 35)
}

func TestAllCommandsStartWithSlash(t *testing.T) {
	allCmds := []string{
		CmdNewBot, CmdMyBots, CmdConnect, CmdDisconnect,
		CmdSetName, CmdSetDescription, CmdDeleteBot,
		CmdToken, CmdRevoke, CmdCancel, CmdHelp, CmdStart,
	}
	for _, cmd := range allCmds {
		assert.True(t, strings.HasPrefix(cmd, "/"), "command %q should start with /", cmd)
		assert.Greater(t, len(cmd), 1, "command should not be just /")
	}
}

func TestStateTTL_Reasonable(t *testing.T) {
	// 状态 TTL 应在合理范围内（1-30分钟）
	assert.GreaterOrEqual(t, stateTTL, 60, "stateTTL should be at least 60 seconds")
	assert.LessOrEqual(t, stateTTL, 1800, "stateTTL should be at most 30 minutes")
}

func TestHeartbeatTTL_Reasonable(t *testing.T) {
	// 心跳 TTL 应在合理范围内（30秒-5分钟）
	assert.GreaterOrEqual(t, heartbeatTTL, 30, "heartbeatTTL should be at least 30 seconds")
	assert.LessOrEqual(t, heartbeatTTL, 300, "heartbeatTTL should be at most 5 minutes")
}
