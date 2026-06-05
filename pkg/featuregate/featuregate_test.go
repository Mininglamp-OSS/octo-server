package featuregate

import "testing"

func TestEvaluate(t *testing.T) {
	const key = "incoming_webhook_create"
	groupScopes := []Scope{
		{Type: ScopeTypeGroup, ID: "g_allow"},
		{Type: ScopeTypeSpace, ID: "sp_allow"}, // space 条目当前应被忽略
	}

	tests := []struct {
		name       string
		rule       Rule
		scopes     []Scope
		dims       Dims
		wantAllow  bool
		wantReason string
	}{
		{
			name:       "off 全拒",
			rule:       Rule{Key: key, Mode: ModeOff},
			dims:       Dims{GroupNo: "g_allow"},
			wantAllow:  false,
			wantReason: ReasonOff,
		},
		{
			name:       "on 全放",
			rule:       Rule{Key: key, Mode: ModeOn},
			dims:       Dims{GroupNo: "whatever"},
			wantAllow:  true,
			wantReason: ReasonOn,
		},
		{
			name:       "whitelist group 命中",
			rule:       Rule{Key: key, Mode: ModeWhitelist},
			scopes:     groupScopes,
			dims:       Dims{GroupNo: "g_allow"},
			wantAllow:  true,
			wantReason: ReasonWhitelistHit,
		},
		{
			name:       "whitelist group 未命中",
			rule:       Rule{Key: key, Mode: ModeWhitelist},
			scopes:     groupScopes,
			dims:       Dims{GroupNo: "g_other"},
			wantAllow:  false,
			wantReason: ReasonWhitelistMiss,
		},
		{
			name:       "whitelist 仅认 group：space 命中不放行",
			rule:       Rule{Key: key, Mode: ModeWhitelist},
			scopes:     groupScopes,
			dims:       Dims{SpaceID: "sp_allow", GroupNo: "g_other"},
			wantAllow:  false,
			wantReason: ReasonWhitelistMiss,
		},
		{
			name:       "whitelist 空 GroupNo 不匹配任何条目",
			rule:       Rule{Key: key, Mode: ModeWhitelist},
			scopes:     groupScopes,
			dims:       Dims{GroupNo: ""},
			wantAllow:  false,
			wantReason: ReasonWhitelistMiss,
		},
		{
			name:       "percent=0 全拒",
			rule:       Rule{Key: key, Mode: ModePercent, Percent: 0},
			dims:       Dims{GroupNo: "g_allow"},
			wantAllow:  false,
			wantReason: ReasonPercentOut,
		},
		{
			name:       "percent=100 全放",
			rule:       Rule{Key: key, Mode: ModePercent, Percent: 100},
			dims:       Dims{GroupNo: "g_allow"},
			wantAllow:  true,
			wantReason: ReasonPercentIn,
		},
		{
			name:       "未知 mode 保守拒绝",
			rule:       Rule{Key: key, Mode: Mode("bogus")},
			dims:       Dims{GroupNo: "g_allow"},
			wantAllow:  false,
			wantReason: ReasonUnknownMode,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.rule, tt.scopes, tt.dims)
			if got.Allow != tt.wantAllow || got.Reason != tt.wantReason {
				t.Fatalf("Evaluate() = {Allow:%v Reason:%q}, want {Allow:%v Reason:%q}",
					got.Allow, got.Reason, tt.wantAllow, tt.wantReason)
			}
		})
	}
}

// TestEvaluatePercentBoundary 用实际分桶值验证 percent 边界：阈值恰等于桶值时
// 不命中（Bucket < percent 为严格小于），阈值比桶值大 1 时命中。无需 hardcode
// 具体桶值，保证测试不随 hash 实现细节脆裂。
func TestEvaluatePercentBoundary(t *testing.T) {
	const key = "incoming_webhook_create"
	const group = "g_boundary"
	b := Bucket(key, group)

	if b > 0 {
		atThreshold := Evaluate(Rule{Key: key, Mode: ModePercent, Percent: b}, nil, Dims{GroupNo: group})
		if atThreshold.Allow {
			t.Fatalf("percent=bucket(%d) 应不命中（严格小于），却放行了", b)
		}
	}
	if b < 100 {
		aboveThreshold := Evaluate(Rule{Key: key, Mode: ModePercent, Percent: b + 1}, nil, Dims{GroupNo: group})
		if !aboveThreshold.Allow {
			t.Fatalf("percent=bucket+1(%d) 应命中，却被拒绝", b+1)
		}
	}
}

// TestBucketStable 验证分桶稳定（同入参恒定）且落在 [0,100)。
func TestBucketStable(t *testing.T) {
	cases := []struct{ key, id string }{
		{"incoming_webhook_create", "g_1"},
		{"incoming_webhook_create", "g_2"},
		{"another_feature", "g_1"},
		{"k", ""},
	}
	for _, c := range cases {
		first := Bucket(c.key, c.id)
		if first < 0 || first >= 100 {
			t.Fatalf("Bucket(%q,%q)=%d 超出 [0,100)", c.key, c.id, first)
		}
		for i := 0; i < 5; i++ {
			if again := Bucket(c.key, c.id); again != first {
				t.Fatalf("Bucket(%q,%q) 不稳定：%d != %d", c.key, c.id, again, first)
			}
		}
	}

	// 不同 key 对同一 id 应相互独立（加盐生效）：用一个已知会分到不同桶的样本，
	// 避免「所有 key 同桶」这种退化实现悄悄通过。
	if Bucket("incoming_webhook_create", "g_1") == Bucket("incoming_webhook_create_x", "g_1") {
		// 极小概率自然相等，不直接失败，仅提示；改用第二组样本兜底。
		if Bucket("feature_a", "same") == Bucket("feature_b", "same") &&
			Bucket("feature_c", "same") == Bucket("feature_d", "same") {
			t.Fatalf("不同 key 的分桶高度疑似未加盐（多组样本均相等）")
		}
	}
}
