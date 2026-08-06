package common

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
)

// SettingBoolOK 的解析规则测试（round-3 评审 P2-3）。
//
// 无基础设施：直接塞 snapshot，同 botRLSettings 的模式。
func boolOKSettings(value string) *SystemSettings {
	s := &SystemSettings{Log: log.NewTLog("SystemSettingsTest")}
	m := map[string]string{schemaKey("botcard", "display_enabled"): value}
	s.snapshot.Store(&m)
	return s
}

// TestSettingBoolOK_LexingIsForgiving —— 大小写与首尾空白不得让一次配置「消失」。
//
// 这条守的是**失败方向**而非美观。SettingBoolOK 的调用方会在其上再叠一层代码默认，
// 而今天用它的三个键（Bot 卡片开关）代码默认全是 true。所以运维在 system_setting
// 里写下 `False` 却被判成「未配置」时，得到的不是一个中性的回落，而是**他本想关掉
// 的能力继续开着**，且全链路没有任何报错。
func TestSettingBoolOK_LexingIsForgiving(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		value bool
	}{
		{"true", true}, {"TRUE", true}, {"True", true}, {"1", true}, {" true ", true},
		{"false", false}, {"FALSE", false}, {"False", false}, {"0", false}, {"\tfalse\n", false},
	} {
		value, configured := boolOKSettings(tc.raw).SettingBoolOK("botcard", "display_enabled")
		assert.True(t, configured, "%q 必须被认作已配置", tc.raw)
		assert.Equal(t, tc.value, value, "%q 解析成了 %v", tc.raw, value)
	}
}

// TestSettingBoolOK_VocabularyMatchesParseSettingBool —— 词表故意不扩张。
//
// 放宽的只是词法（trim + 大小写），不是词表。把 on/off/yes/no 也收进来，会让
// system_setting 的同一列对两个读者（getBool / SettingBoolOK）含义不同——这正是
// 本模块要避免的分叉，而不是顺手的便利。任何一天要加词，两处一起加。
func TestSettingBoolOK_VocabularyMatchesParseSettingBool(t *testing.T) {
	for _, raw := range []string{"on", "off", "yes", "no", "enabled", "2", "null"} {
		_, configured := boolOKSettings(raw).SettingBoolOK("botcard", "display_enabled")
		assert.False(t, configured, "%q 不该被 SettingBoolOK 单方面接受", raw)

		// 同一个词在 getBool 侧同样落 fallback —— 两个读者始终同词表。
		assert.True(t, parseSettingBool(raw, true), "%q 在 getBool 侧被解析了，两处词表已分叉", raw)
		assert.False(t, parseSettingBool(raw, false), "%q 在 getBool 侧被解析了，两处词表已分叉", raw)
	}
}

// TestSettingBoolOK_UnsetAndUnparseableBothFallThrough —— 「没配」与「配错了」在
// 本层同样报 configured=false（调用方据此回落），区别只在日志：脏值由调用方
// （modules/robot 的 bot_setting 解析器）告警，本层不知道调用方的期望。
func TestSettingBoolOK_UnsetAndUnparseableBothFallThrough(t *testing.T) {
	value, configured := boolOKSettings("").SettingBoolOK("botcard", "display_enabled")
	assert.False(t, configured)
	assert.False(t, value)

	value, configured = boolOKSettings("maybe").SettingBoolOK("botcard", "display_enabled")
	assert.False(t, configured)
	assert.False(t, value)
}
