package featuregate

import (
	"fmt"
	"testing"
)

// TestEvaluate 覆盖四个 mode × 三个维度的判定矩阵。每个用例都断言 Reason，
// 因为 Reason 是运维排障时唯一能区分「白名单进来的」「分桶进来的」「维度缺失
// 被拒的」的信号，回归时静默改掉 Reason 与改掉 Allow 同样有害。
func TestEvaluate(t *testing.T) {
	userScope := []Scope{{Type: ScopeTypeUser, ID: "u1"}}
	groupScope := []Scope{{Type: ScopeTypeGroup, ID: "g1"}}
	spaceScope := []Scope{{Type: ScopeTypeSpace, ID: "s1"}}

	cases := []struct {
		name       string
		rule       Rule
		scopes     []Scope
		dims       Dims
		wantAllow  bool
		wantReason string
	}{
		// ---- off：无条件关，白名单不可穿透 ----
		{
			name:       "off 全拒",
			rule:       Rule{Key: "k", Mode: ModeOff},
			dims:       Dims{UID: "u1"},
			wantAllow:  false,
			wantReason: ReasonOff,
		},
		{
			name:       "off 不可被用户白名单穿透（止血语义的硬边界）",
			rule:       Rule{Key: "k", Mode: ModeOff},
			scopes:     userScope,
			dims:       Dims{UID: "u1"},
			wantAllow:  false,
			wantReason: ReasonOff,
		},
		{
			name:       "off 不可被群白名单穿透",
			rule:       Rule{Key: "k", Mode: ModeOff},
			scopes:     groupScope,
			dims:       Dims{GroupNo: "g1"},
			wantAllow:  false,
			wantReason: ReasonOff,
		},

		// ---- on ----
		{
			name:       "on 全放",
			rule:       Rule{Key: "k", Mode: ModeOn},
			dims:       Dims{},
			wantAllow:  true,
			wantReason: ReasonOn,
		},

		// ---- whitelist：user 维度（本任务新增） ----
		{
			name:       "whitelist 用户命中",
			rule:       Rule{Key: "k", Mode: ModeWhitelist},
			scopes:     userScope,
			dims:       Dims{UID: "u1"},
			wantAllow:  true,
			wantReason: ReasonWhitelistHit,
		},
		{
			name:       "whitelist 用户未命中",
			rule:       Rule{Key: "k", Mode: ModeWhitelist},
			scopes:     userScope,
			dims:       Dims{UID: "u2"},
			wantAllow:  false,
			wantReason: ReasonWhitelistMiss,
		},
		{
			// 「空 UID 不命中」的保证仍在（Allow=false）。Reason 是 dim_unavailable：
			// 名单里全是 user 条目而这次调用没有 UID —— 没法判，不是判了没中。
			// 与 api_flags.go 对空 uid 打 Error 的处理同调。
			name:       "whitelist 空 UID 不命中任何用户条目，且错配可见",
			rule:       Rule{Key: "k", Mode: ModeWhitelist},
			scopes:     userScope,
			dims:       Dims{UID: ""},
			wantAllow:  false,
			wantReason: ReasonWhitelistDimUnavailable,
		},
		{
			// 报 dim_unavailable 而非 miss 是刻意的：UID 本身取不到值，这是
			// 「没法判」不是「判了没中」。脏数据防御（空 scope_id 不匹配空维度）
			// 由 Allow=false 保证，与 Reason 取值无关。
			name:       "whitelist 空 scope_id 不匹配空维度（脏数据防御）",
			rule:       Rule{Key: "k", Mode: ModeWhitelist},
			scopes:     []Scope{{Type: ScopeTypeUser, ID: ""}},
			dims:       Dims{UID: ""},
			wantAllow:  false,
			wantReason: ReasonWhitelistDimUnavailable,
		},
		{
			// 维度不交叉的保证仍在（Allow=false）；Reason 是 dim_unavailable 因为
			// 这次调用根本没有 UID 维度可比。
			name:       "whitelist 维度不交叉：用户条目不会被同值的 group_no 命中",
			rule:       Rule{Key: "k", Mode: ModeWhitelist},
			scopes:     userScope,
			dims:       Dims{GroupNo: "u1"},
			wantAllow:  false,
			wantReason: ReasonWhitelistDimUnavailable,
		},

		// ---- whitelist：group / space 维度 ----
		{
			name:       "whitelist 群命中",
			rule:       Rule{Key: "k", Mode: ModeWhitelist},
			scopes:     groupScope,
			dims:       Dims{GroupNo: "g1"},
			wantAllow:  true,
			wantReason: ReasonWhitelistHit,
		},
		{
			name:       "whitelist 空间命中（初版写侧接受但读侧忽略，此处已打通）",
			rule:       Rule{Key: "k", Mode: ModeWhitelist},
			scopes:     spaceScope,
			dims:       Dims{SpaceID: "s1"},
			wantAllow:  true,
			wantReason: ReasonWhitelistHit,
		},
		{
			// 三个维度都有值，唯独 "tenant" 这个维度不存在 —— 该条目永远命不中。
			// 报 dim_unavailable 让这类配置错误可见，比静默 miss 好。
			name:       "whitelist 未知 scope_type 永不命中，且错配可见",
			rule:       Rule{Key: "k", Mode: ModeWhitelist},
			scopes:     []Scope{{Type: "tenant", ID: "t1"}},
			dims:       Dims{UID: "t1", GroupNo: "t1", SpaceID: "t1"},
			wantAllow:  false,
			wantReason: ReasonWhitelistDimUnavailable,
		},

		// ---- percent：白名单优先（本任务修的语义） ----
		{
			// Percent: 0 保证放行只可能来自白名单，排除分桶巧合。
			name:       "percent 0% 下白名单仍然命中",
			rule:       Rule{Key: "k", Mode: ModePercent, Percent: 0, BucketBy: ScopeTypeUser},
			scopes:     userScope,
			dims:       Dims{UID: "u1"},
			wantAllow:  true,
			wantReason: ReasonWhitelistHit,
		},
		{
			name:       "percent 0% 下非白名单被拒",
			rule:       Rule{Key: "k", Mode: ModePercent, Percent: 0, BucketBy: ScopeTypeUser},
			scopes:     userScope,
			dims:       Dims{UID: "u2"},
			wantAllow:  false,
			wantReason: ReasonPercentOut,
		},
		{
			// 白名单维度与分桶维度可以不同：按群豁免、按用户放量。
			name:       "percent 按用户分桶时群白名单仍生效",
			rule:       Rule{Key: "k", Mode: ModePercent, Percent: 0, BucketBy: ScopeTypeUser},
			scopes:     groupScope,
			dims:       Dims{GroupNo: "g1", UID: "whoever"},
			wantAllow:  true,
			wantReason: ReasonWhitelistHit,
		},

		// ---- percent：边界与维度可用性 ----
		{
			name:       "percent 100% 全放，且不要求维度可用",
			rule:       Rule{Key: "k", Mode: ModePercent, Percent: 100, BucketBy: ScopeTypeGroup},
			dims:       Dims{UID: "u1"}, // 无 GroupNo
			wantAllow:  true,
			wantReason: ReasonPercentIn,
		},
		{
			name:       "percent 负数视同 0",
			rule:       Rule{Key: "k", Mode: ModePercent, Percent: -1, BucketBy: ScopeTypeUser},
			dims:       Dims{UID: "u1"},
			wantAllow:  false,
			wantReason: ReasonPercentOut,
		},
		{
			// 这是「管理台显示 50%、实际全体开或全体关」那个静默错配的防线。
			name:       "percent 分桶维度缺失时 fail-closed 且原因可辨",
			rule:       Rule{Key: "k", Mode: ModePercent, Percent: 50, BucketBy: ScopeTypeGroup},
			dims:       Dims{UID: "u1"}, // 只有 UID，配置却要按 group 分桶
			wantAllow:  false,
			wantReason: ReasonDimUnavailable,
		},
		{
			name:       "percent 未知 bucket_by 同样 fail-closed，不回退到别的维度",
			rule:       Rule{Key: "k", Mode: ModePercent, Percent: 50, BucketBy: "tenant"},
			dims:       Dims{UID: "u1", GroupNo: "g1", SpaceID: "s1"},
			wantAllow:  false,
			wantReason: ReasonDimUnavailable,
		},

		// ---- 未知 mode ----
		{
			name:       "未知 mode 保守拒绝",
			rule:       Rule{Key: "k", Mode: Mode("rollout")},
			dims:       Dims{UID: "u1"},
			wantAllow:  false,
			wantReason: ReasonUnknownMode,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.rule, tc.scopes, tc.dims)
			if got.Allow != tc.wantAllow || got.Reason != tc.wantReason {
				t.Fatalf("Evaluate = {Allow:%v Reason:%q}, want {Allow:%v Reason:%q}",
					got.Allow, got.Reason, tc.wantAllow, tc.wantReason)
			}
		})
	}
}

// TestBucketDimensionDefault 钉住「bucket_by 为空 == 按 group 分桶」这个缺省语义。
func TestBucketDimensionDefault(t *testing.T) {
	if got := BucketDimension(Rule{Key: "k"}); got != ScopeTypeGroup {
		t.Fatalf("空 BucketBy 应归一到 %q，得到 %q", ScopeTypeGroup, got)
	}
	if got := BucketDimension(Rule{Key: "k", BucketBy: ScopeTypeUser}); got != ScopeTypeUser {
		t.Fatalf("显式 BucketBy 应原样返回，得到 %q", got)
	}
}

// TestPercentEmptyBucketByEqualsGroup 验证未指定 bucket_by 的规则与显式
// bucket_by=group 的规则判定完全一致——这是缺省值语义的回归防线。
func TestPercentEmptyBucketByEqualsGroup(t *testing.T) {
	for i := 0; i < 200; i++ {
		groupNo := fmt.Sprintf("g%d", i)
		dims := Dims{GroupNo: groupNo, UID: "u", SpaceID: "s"}
		implicit := Evaluate(Rule{Key: "k", Mode: ModePercent, Percent: 50}, nil, dims)
		explicit := Evaluate(Rule{Key: "k", Mode: ModePercent, Percent: 50, BucketBy: ScopeTypeGroup}, nil, dims)
		if implicit != explicit {
			t.Fatalf("group_no=%s: 隐式 %v != 显式 %v", groupNo, implicit, explicit)
		}
	}
}

// TestBucketStable 验证同一 (key, dimID) 分桶恒定。
func TestBucketStable(t *testing.T) {
	first := Bucket("k", "u1")
	for i := 0; i < 100; i++ {
		if got := Bucket("k", "u1"); got != first {
			t.Fatalf("第 %d 次 Bucket = %d，首次 %d：分桶必须恒定", i, got, first)
		}
	}
	if first < 0 || first >= 100 {
		t.Fatalf("Bucket 必须落在 [0,100)，得到 %d", first)
	}
}

// TestBucketIndependentAcrossKeys 验证按 key 加盐生效：同一个 uid 在不同功能 key
// 下不会同涨同落。断言「不是全部相同」而非某个具体值，避免把哈希实现钉死。
func TestBucketIndependentAcrossKeys(t *testing.T) {
	const uid = "u1"
	seen := make(map[int]struct{})
	for i := 0; i < 32; i++ {
		seen[Bucket(fmt.Sprintf("feature_%d", i), uid)] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("32 个不同 key 对同一 uid 只产生 %d 个桶值，按 key 加盐未生效", len(seen))
	}
}

// TestPercentMonotonic 验证放量单调性：percent 调高只会纳入更多对象，已命中的
// 不会掉出。直接取该 (key,uid) 的真实桶值做边界断言，比抽样更严格。
func TestPercentMonotonic(t *testing.T) {
	const key, uid = "k", "u1"
	b := Bucket(key, uid)

	allowedAt := func(p int) bool {
		return Evaluate(Rule{Key: key, Mode: ModePercent, Percent: p, BucketBy: ScopeTypeUser},
			nil, Dims{UID: uid}).Allow
	}

	// 边界：桶值 b 在 percent=b 时落在窗口外，percent=b+1 时进入。
	if allowedAt(b) {
		t.Fatalf("桶值 %d 在 percent=%d 时不应命中（判据是 bucket < percent）", b, b)
	}
	if !allowedAt(b + 1) {
		t.Fatalf("桶值 %d 在 percent=%d 时应当命中", b, b+1)
	}
	// 单调：一旦命中，继续调高不得掉出。
	for p := b + 1; p <= 100; p++ {
		if !allowedAt(p) {
			t.Fatalf("percent 从 %d 调到 %d 时对象掉出，违反单调性", b+1, p)
		}
	}
}

// TestValidDimension 钉住写侧校验的取值域。空串视为非法：调用方要表达「未配置」
// 应在传入前判空，而不是把空串当合法值存库。
func TestValidDimension(t *testing.T) {
	for _, ok := range []string{ScopeTypeGroup, ScopeTypeSpace, ScopeTypeUser} {
		if !ValidDimension(ok) {
			t.Fatalf("%q 应为合法维度", ok)
		}
	}
	for _, bad := range []string{"", "tenant", "Group", "user "} {
		if ValidDimension(bad) {
			t.Fatalf("%q 不应为合法维度", bad)
		}
	}
}

// TestWhitelistDimUnavailableIsDistinguishable 钉住读侧对「白名单条目全都用不上」的
// 可辨识性。
//
// 这是评审抓到的那个盲区：一条 client-visible 规则的白名单若全是 group 条目，展示
// 端点（只有 UID）跳过每一条后返回 whitelist_miss —— 与「用户确实不在名单里」完全
// 同形。运维看到的是白名单有行、mode 是 whitelist、写入都 200、日志无声，而 flag 对
// 所有人是 false。bucket_by 那条错配有 ReasonDimUnavailable 兜底，这条一直没有。
func TestWhitelistDimUnavailableIsDistinguishable(t *testing.T) {
	cases := []struct {
		name       string
		scopes     []Scope
		dims       Dims
		wantReason string
	}{
		{
			// 全部条目的维度都取不到值 —— 这不是"没命中"，是"根本没法命中"。
			name:       "条目维度全不可用",
			scopes:     []Scope{{Type: ScopeTypeGroup, ID: "g1"}, {Type: ScopeTypeSpace, ID: "s1"}},
			dims:       Dims{UID: "u1"},
			wantReason: ReasonWhitelistDimUnavailable,
		},
		{
			// 有可用维度的条目，只是没匹配上 —— 这是真正的 miss。
			name:       "维度可用但未匹配",
			scopes:     []Scope{{Type: ScopeTypeUser, ID: "someone_else"}},
			dims:       Dims{UID: "u1"},
			wantReason: ReasonWhitelistMiss,
		},
		{
			// 混合：只要有一条维度可用，就是普通 miss，不该报维度不可用。
			name:       "混合维度，有一条可用",
			scopes:     []Scope{{Type: ScopeTypeGroup, ID: "g1"}, {Type: ScopeTypeUser, ID: "other"}},
			dims:       Dims{UID: "u1"},
			wantReason: ReasonWhitelistMiss,
		},
		{
			// 空白名单是合法状态（mode=whitelist 且谁都不放），不是错配。
			name:       "空白名单仍是普通 miss",
			scopes:     nil,
			dims:       Dims{UID: "u1"},
			wantReason: ReasonWhitelistMiss,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(Rule{Key: "k", Mode: ModeWhitelist}, tc.scopes, tc.dims)
			if got.Allow {
				t.Fatalf("均不应放行，得到 Allow=true")
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// TestPercentShortcutStillReportsUnusableDimension 钉住 percent 的 0%/100% 短路
// **不改变决策**、但要让错配可被上层发现。
//
// 两位评审在这里意见相左：一位主张把维度校验提到 100% 短路之前（即 fail-closed），
// 另一位主张决策是对的、只需可见。取后者——percent=100 的意图无歧义（"所有人"），
// 管理台显示与实际一致，为一个不影响结果的错配去打挂一条正在全量的规则是本末倒置。
// 但错配必须留下痕迹，否则运维会在把 100 调到 50 的那一刻突然全员掉线。
func TestPercentShortcutStillReportsUnusableDimension(t *testing.T) {
	// 展示端点只有 UID，规则却配了 group 分桶。
	dims := Dims{UID: "u1"}

	full := Evaluate(Rule{Key: "k", Mode: ModePercent, Percent: 100, BucketBy: ScopeTypeGroup}, nil, dims)
	if !full.Allow {
		t.Fatalf("100%% 必须放行（决策不因维度错配而改变），得到 %+v", full)
	}
	if !full.DimensionUnusable {
		t.Fatalf("100%% 短路仍须把维度不可用回报给上层，得到 %+v", full)
	}

	zero := Evaluate(Rule{Key: "k", Mode: ModePercent, Percent: 0, BucketBy: ScopeTypeGroup}, nil, dims)
	if zero.Allow {
		t.Fatalf("0%% 必须拒绝")
	}
	if !zero.DimensionUnusable {
		t.Fatalf("0%% 短路同样须回报维度不可用，得到 %+v", zero)
	}

	// 维度可用时不得误报。
	ok := Evaluate(Rule{Key: "k", Mode: ModePercent, Percent: 100, BucketBy: ScopeTypeUser}, nil, dims)
	if ok.DimensionUnusable {
		t.Fatalf("维度可用时不应报 DimensionUnusable，得到 %+v", ok)
	}
}
