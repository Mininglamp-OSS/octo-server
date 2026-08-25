package featuregate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/common"
)

func TestNewClientRegistryValidation(t *testing.T) {
	cases := []struct {
		name    string
		flags   []ClientFlag
		wantErr string // 空 = 应当成功
	}{
		{
			name:  "空注册表合法",
			flags: []ClientFlag{},
		},
		{
			name: "正常两项",
			flags: []ClientFlag{
				{FeatureKey: "docs_user_rollout", ClientKey: "docs"},
				{FeatureKey: "drive_beta", ClientKey: "drive_beta"},
			},
		},
		{
			// 静默故障防线：响应是 map[string]bool，两项同 ClientKey 时后写覆盖
			// 先写，map 不报错，线上表现为「某 flag 的值跟着另一个功能走」。
			name: "重复 ClientKey 必须报错",
			flags: []ClientFlag{
				{FeatureKey: "a_rollout", ClientKey: "same"},
				{FeatureKey: "b_rollout", ClientKey: "same"},
			},
			wantErr: "duplicate ClientKey",
		},
		{
			name: "重复 FeatureKey 必须报错",
			flags: []ClientFlag{
				{FeatureKey: "same_key", ClientKey: "a"},
				{FeatureKey: "same_key", ClientKey: "b"},
			},
			wantErr: "duplicate FeatureKey",
		},
		{
			name:    "ClientKey 必须 snake_case",
			flags:   []ClientFlag{{FeatureKey: "ok_key", ClientKey: "DocsOn"}},
			wantErr: "invalid ClientKey",
		},
		{
			name:    "ClientKey 不得含连字符（JSON 字段风格对齐 appconfig）",
			flags:   []ClientFlag{{FeatureKey: "ok_key", ClientKey: "docs-on"}},
			wantErr: "invalid ClientKey",
		},
		{
			// 连字符会让 key → env 名不再是单射（见 KillSwitchEnv）。
			name:    "FeatureKey 不得含连字符",
			flags:   []ClientFlag{{FeatureKey: "docs-beta", ClientKey: "docs_beta"}},
			wantErr: "invalid FeatureKey",
		},
		{
			name:    "FeatureKey 不得含空格（否则杀开关环境变量名设不进去）",
			flags:   []ClientFlag{{FeatureKey: "my gate", ClientKey: "ok"}},
			wantErr: "invalid FeatureKey",
		},
		{
			name:    "空 ClientKey 非法",
			flags:   []ClientFlag{{FeatureKey: "ok_key", ClientKey: ""}},
			wantErr: "invalid ClientKey",
		},
		{
			name:    "空 FeatureKey 非法",
			flags:   []ClientFlag{{FeatureKey: "", ClientKey: "ok"}},
			wantErr: "invalid FeatureKey",
		},
		{
			// 超长 key 写库时才失败太晚 —— 在注册期就挡住。
			name:    "FeatureKey 超过 DB 列长度非法",
			flags:   []ClientFlag{{FeatureKey: strings.Repeat("a", maxFeatureKeyLen+1), ClientKey: "ok"}},
			wantErr: "exceeds",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := newClientRegistry(tc.flags)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望成功，得到错误：%v", err)
				}
				if r == nil {
					t.Fatal("成功时 registry 不应为 nil")
				}
				return
			}
			if err == nil {
				t.Fatalf("期望错误包含 %q，却成功了", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 %q 未包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestMustNewClientRegistryPanics 钉住注册表写错时是启动期 panic，而不是带着一个
// 会静默串值的表上线。
func TestMustNewClientRegistryPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("重复 ClientKey 必须 panic")
		}
	}()
	mustNewClientRegistry([]ClientFlag{
		{FeatureKey: "a_rollout", ClientKey: "same"},
		{FeatureKey: "b_rollout", ClientKey: "same"},
	})
}

// TestRegistryNeedsSettings 验证「只在真有 flag 声明部署前置位时才去取
// SystemSettings 单例」的判定。取那个单例会做一次 DB Load 并起后台 goroutine，
// 没人用就不该付这个代价。
func TestRegistryNeedsSettings(t *testing.T) {
	plain := mustNewClientRegistry([]ClientFlag{{FeatureKey: "a_rollout", ClientKey: "a"}})
	if plain.needsSettings {
		t.Fatal("无 DeploymentGate 时不应要求 SystemSettings")
	}
	gated := mustNewClientRegistry([]ClientFlag{
		{FeatureKey: "a_rollout", ClientKey: "a"},
		{FeatureKey: "b_rollout", ClientKey: "b", DeploymentGate: func(*common.SystemSettings) bool { return true }},
	})
	if !gated.needsSettings {
		t.Fatal("有 DeploymentGate 时必须要求 SystemSettings")
	}
}

func TestRegistryIsClientVisible(t *testing.T) {
	r := mustNewClientRegistry([]ClientFlag{{FeatureKey: "docs_user_rollout", ClientKey: "docs"}})
	if !r.isClientVisible("docs_user_rollout") {
		t.Fatal("已注册的 feature_key 应为 client-visible")
	}
	// 按 feature_key 判定，不是 client_key —— 管理端拿到的是前者。
	if r.isClientVisible("docs") {
		t.Fatal("client_key 不应被当成 feature_key 命中")
	}
	if r.isClientVisible("unregistered") {
		t.Fatal("未注册的 key 不应为 client-visible")
	}
}

// TestDefaultRegistryIsValid 确保生产清单本身能通过校验。清单当前为空（本任务交付
// 机制而非具体业务开关），但这条测试会在有人追加条目后继续守住唯一性与命名。
func TestDefaultRegistryIsValid(t *testing.T) {
	if _, err := newClientRegistry(clientFlagList); err != nil {
		t.Fatalf("生产注册表非法：%v", err)
	}
}

// TestRegistryIsImmutableAfterConstruction 钉住注册表构造后不可被绕过校验地改写。
// 直接持有调用方 slice 时，对其元素的原地修改会穿透进已校验的注册表。
func TestRegistryIsImmutableAfterConstruction(t *testing.T) {
	src := []ClientFlag{{FeatureKey: "a_rollout", ClientKey: "a"}}
	r := mustNewClientRegistry(src)

	src[0].FeatureKey = "hijacked"
	src[0].ClientKey = "hijacked"

	if !r.isClientVisible("a_rollout") {
		t.Fatal("注册表被外部改写：构造时必须拷贝一份，而不是持有调用方的 slice")
	}
	if r.flags[0].ClientKey != "a" {
		t.Fatalf("ClientKey 被外部改写为 %q，绕过了构造期的唯一性/命名校验", r.flags[0].ClientKey)
	}
}

// TestValidFeatureKey 是 feature_key 的单一校验口径，注册表与管理端写路径共用。
func TestValidFeatureKey(t *testing.T) {
	for _, ok := range []string{"a", "docs_beta", "incoming_webhook_create", "a1_2"} {
		if !validFeatureKey(ok) {
			t.Fatalf("%q 应为合法 feature_key", ok)
		}
	}
	for _, bad := range []string{
		"",                                      // 空
		"1abc",                                  // 数字开头
		"Docs",                                  // 大写
		"docs-beta",                             // 连字符 → 破坏 key→env 单射
		"my gate",                               // 空格 → 杀开关环境变量名设不进去
		"docs.beta",                             // 点
		"docs/beta",                             // 斜杠
		"docs\nbeta",                            // 换行
		strings.Repeat("a", maxFeatureKeyLen+1), // 超 DB 列长
	} {
		if validFeatureKey(bad) {
			t.Fatalf("%q 不应为合法 feature_key", bad)
		}
	}
}

// TestKillSwitchEnvIsInjective 钉住 feature_key → env 名的单射性。
//
// 早先 featureKeyPattern 允许连字符、KillSwitchEnv 把 `-` 折成 `_`，于是
// docs-beta 与 docs_beta 这两条不同的规则会共用一个 OCTO_FEATUREGATE_DOCS_BETA_KILL ——
// 停一个会把另一个一起停掉。现在连字符被禁，且推导不做任何折叠。
func TestKillSwitchEnvIsInjective(t *testing.T) {
	keys := []string{"docs_beta", "docsbeta", "a", "a_b", "ab", "incoming_webhook_push"}
	seen := make(map[string]string, len(keys))
	for _, k := range keys {
		if !validFeatureKey(k) {
			t.Fatalf("用例 key %q 本身应合法", k)
		}
		env := KillSwitchEnv(k)
		if prev, dup := seen[env]; dup {
			t.Fatalf("key %q 与 %q 映射到同一个杀开关 %q：停一个会误停另一个", k, prev, env)
		}
		seen[env] = k
	}
	if got := KillSwitchEnv("docs_beta"); got != "OCTO_FEATUREGATE_DOCS_BETA_KILL" {
		t.Fatalf("KillSwitchEnv 推导有变：%q", got)
	}
}

// TestFlagsRespSerialization 钉住 wire 形状：动态 map、且值为 false 的 key 必须
// 出现在 JSON 里。
//
// 这不是风格测试。若有人把 Flags 换成固定字段 struct，「省略某个 key」就实现不了；
// 若为了实现省略而加 omitempty，false 会被一起吞掉——「规则不存在的确定性的关」
// 与「存储故障」在线上就长得一模一样，客户端会保留旧值，灰度永远关不掉。
func TestFlagsRespSerialization(t *testing.T) {
	raw, err := json.Marshal(flagsResp{Flags: map[string]bool{"on_flag": true, "off_flag": false}, Unavailable: []string{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back struct {
		Flags map[string]bool `json:"flags"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := back.Flags["off_flag"]
	if !ok {
		t.Fatalf("值为 false 的 key 被从 JSON 中吞掉了（omitempty？）：%s", raw)
	}
	if v {
		t.Fatalf("off_flag 应为 false，得到 true：%s", raw)
	}
	if !back.Flags["on_flag"] {
		t.Fatalf("on_flag 应为 true：%s", raw)
	}
}

// TestFlagsRespEmptyMapSerializesAsObject 确保空注册表下响应是 {"flags":{}} 而不是
// {"flags":null} —— 后者会让弱类型客户端在解引用时炸掉。
func TestFlagsRespEmptyMapSerializesAsObject(t *testing.T) {
	raw, err := json.Marshal(flagsResp{Flags: map[string]bool{}, Unavailable: []string{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(raw); got != `{"flags":{},"unavailable":[]}` {
		t.Fatalf("空响应应序列化为 {\"flags\":{},\"unavailable\":[]}，得到 %s", got)
	}
}
