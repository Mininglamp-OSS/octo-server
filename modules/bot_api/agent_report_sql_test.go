package bot_api

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// SQL-level tests for updateRobotAgentInfo —— 直接断言**发出的语句**里有哪些列。
//
// 为什么需要这一层：PR #837 的两位 reviewer 独立指出，「值被保留」这类通过 DB 观察
// 的断言**区分不了**两种实现 —— 被否决的「把缺席字段解析成刚读到的值再写回」会让
// 语句里出现 `col=<旧值>`，读回来一模一样，端到端测试照样全绿。而这两种实现在并发
// 下行为不同（写回会丢更新）。唯一能钉死它的就是断言语句本身。
//
// 用 sqlmock 而非真库，理由同 obo_db_persona_prompt_test.go：这里被测的是 SQL 形状，
// 不是持久化行为。

func newAgentReportDB(t *testing.T) (*botAPIDB, sqlmock.Sqlmock, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return &botAPIDB{session: conn.NewSession(nil), ctx: nil}, mock, func() { _ = rawDB.Close() }
}

func strptr(s string) *string { return &s }

// TestUpdateRobotAgentInfoOmitsUnreportedColumns —— 缺席的字段必须不出现在
// UPDATE 语句里（对四个字段逐一验证）。
func TestUpdateRobotAgentInfoOmitsUnreportedColumns(t *testing.T) {
	cases := []struct {
		name    string
		report  agentReport
		present []string
		absent  []string
		// stored: 库里已有的值，只喂给 changed() 做跳过比较。
		stored agentReport
		// noWrite: 该 report 不该产生任何语句（所有 supplied 字段都不可写）。
		noWrite bool
	}{
		{
			name:    "只报 hosting：三个版本列必须缺席（PR #837 P1-2 的阻塞项）",
			report:  agentReport{Hosting: strptr("self_hosted")},
			present: []string{"agent_hosting", "agent_reported_hosting_at"},
			absent:  []string{"agent_platform", "agent_version", "plugin_version"},
		},
		{
			name:    "只报版本：hosting 与其时间戳都必须缺席",
			report:  agentReport{Version: strptr("1.0.0")},
			present: []string{"agent_version"},
			absent:  []string{"agent_hosting", "agent_reported_hosting_at", "agent_platform", "plugin_version"},
		},
		{
			name:    "只报 platform",
			report:  agentReport{Platform: strptr("OpenClaw")},
			present: []string{"agent_platform"},
			absent:  []string{"agent_version", "plugin_version", "agent_hosting", "agent_reported_hosting_at"},
		},
		{
			name:    "报空 hosting（清空）：列在，时间戳也在",
			report:  agentReport{Hosting: strptr("")},
			present: []string{"agent_hosting", "agent_reported_hosting_at"},
			absent:  []string{"agent_platform", "agent_version", "plugin_version"},
		},
		{
			name:    "三个 legacy 字段报空串：全部跳过（保留已存值，PR #837 round 2 P1-1）",
			report:  agentReport{Platform: strptr(""), Version: strptr(""), Plugin: strptr("")},
			absent:  []string{"agent_platform", "agent_version", "plugin_version", "agent_hosting", "agent_reported_hosting_at"},
			noWrite: true,
		},
		{
			name:    "legacy 空串与非空混报：只写非空的那个",
			report:  agentReport{Platform: strptr("OpenClaw"), Version: strptr(""), Plugin: strptr("")},
			present: []string{"agent_platform"},
			absent:  []string{"agent_version", "plugin_version"},
		},
		{
			// 撤回（AgentHostingNone 已在 normalizeAgentHosting 折成 ""）：列在、时间戳在。
			// 撤回是一次真实上报，所以 hosting **不做** skip-if-unchanged —— 连续两次撤回
			// 都要推进时间戳。
			name:    "hosting 撤回（归一后为空串）：列在，时间戳也在",
			report:  agentReport{Hosting: strptr(""), Version: strptr("")},
			present: []string{"agent_hosting", "agent_reported_hosting_at"},
			absent:  []string{"agent_version"},
		},
		{
			name:    "legacy 值与库中相同：跳过（不发无可观察效果的空写，round 4 P2-2）",
			report:  agentReport{Version: strptr("1.0.0")},
			stored:  agentReport{Version: strptr("1.0.0")},
			noWrite: true,
		},
		{
			name:    "legacy 值与库中不同：写调用方的值（不是 stored 的）",
			report:  agentReport{Version: strptr("2.0.0")},
			stored:  agentReport{Version: strptr("1.0.0")},
			present: []string{"agent_version"},
		},
		{
			name:    "hosting 相同也照写：撤回/再确认都是一次上报，时间戳必须推进",
			report:  agentReport{Hosting: strptr("self_hosted")},
			stored:  agentReport{Hosting: strptr("self_hosted")},
			present: []string{"agent_hosting", "agent_reported_hosting_at"},
		},
		{
			name:    "全都报：五列俱在",
			report:  agentReport{Platform: strptr("OpenClaw"), Version: strptr("1"), Plugin: strptr("2"), Hosting: strptr("octo_hosted")},
			present: []string{"agent_platform", "agent_version", "plugin_version", "agent_hosting", "agent_reported_hosting_at"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noWrite {
				// 不设任何 Expect：一旦发出语句 sqlmock 就报 "was not expected"。
				dn, mockn, closern := newAgentReportDB(t)
				defer closern()
				require.NoError(t, dn.updateRobotAgentInfo("bot_x", tc.report, tc.stored))
				require.NoError(t, mockn.ExpectationsWereMet())
				return
			}

			d, mock, closer := newAgentReportDB(t)
			defer closer()

			var got string
			mock.ExpectExec("UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
			require.NoError(t, d.updateRobotAgentInfo("bot_x", tc.report, tc.stored))
			require.NoError(t, mock.ExpectationsWereMet())

			// sqlmock 不直接回吐 SQL 文本，改用 QueryMatcher 捕获：重跑一次并抓语句。
			d2, mock2, closer2 := newAgentReportDBCapturing(t, &got)
			defer closer2()
			require.NoError(t, d2.updateRobotAgentInfo("bot_x", tc.report, tc.stored))
			require.NoError(t, mock2.ExpectationsWereMet())

			for _, col := range tc.present {
				assert.Contains(t, got, col, "语句里应包含 %s\nSQL: %s", col, got)
			}
			for _, col := range tc.absent {
				assert.NotContains(t, got, col,
					"缺席的字段 %s 绝不能出现在语句里 —— 写回「读到的值」会在并发下丢更新\nSQL: %s", col, got)
			}
		})
	}
}

// newAgentReportDBCapturing 用自定义 QueryMatcher 抓取实际发出的 SQL 文本。
func newAgentReportDBCapturing(t *testing.T, sink *string) (*botAPIDB, sqlmock.Sqlmock, func()) {
	t.Helper()
	matcher := sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
		*sink = actualSQL
		return nil
	})
	rawDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	mock.ExpectExec("UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return &botAPIDB{session: conn.NewSession(nil), ctx: nil}, mock, func() { _ = rawDB.Close() }
}

// TestUpdateRobotAgentInfoStampsHostingTimeWithSQLNow —— 时间戳必须由 SQL NOW()
// 产生，不能是 Go 侧的 time.Now() 绑定参数。
//
// PR #837 P1-1：Go 侧写入要经驱动的 Config.Loc（默认 UTC，DSN 未设 loc）转换，而
// 应用镜像固定 TZ=Asia/Shanghai —— MySQL session 时区非 UTC 时，这个值会与同一响应
// 里用 NOW() 写的 bound_at 相差 8 小时。断言语句里出现字面的 NOW()，就把「时钟同源」
// 变成了结构约束而不是约定。
func TestUpdateRobotAgentInfoStampsHostingTimeWithSQLNow(t *testing.T) {
	var got string
	d, mock, closer := newAgentReportDBCapturing(t, &got)
	defer closer()

	require.NoError(t, d.updateRobotAgentInfo("bot_x", agentReport{Hosting: strptr("self_hosted")}, agentReport{}))
	require.NoError(t, mock.ExpectationsWereMet())

	// dbr 会给列名加反引号，正则要容得下。
	assert.Regexp(t, regexp.MustCompile("(?i)`?agent_reported_hosting_at`?\\s*=\\s*NOW\\(\\)"), got,
		"时间戳必须是 SQL NOW()（与 bound_at 同源），不能是 Go 侧 time.Now() 的绑定参数\nSQL: %s", got)
}

// TestUpdateRobotAgentInfoNoopOnEmptyReport —— 什么都没报时不发语句。
//
// 若发了，一个空 body 的 register 就会白拿一次行锁；更糟的是「上报」这个概念本身
// 会失真（从未上报过的 bot 也会被盖上时间戳）。
func TestUpdateRobotAgentInfoNoopOnEmptyReport(t *testing.T) {
	d, mock, closer := newAgentReportDB(t)
	defer closer()

	// 不设任何 Expect：一旦发出语句，sqlmock 会报 "call to ExecQuery ... was not expected"。
	require.NoError(t, d.updateRobotAgentInfo("bot_x", agentReport{}, agentReport{}))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------- Warn 断言 ----------
//
// registerAppBot 解析 body 的**唯一**理由就是发这条 Warn（「不发的话上报方永远
// 发现不了自己白报了」）。PR #837 的两位 reviewer 都指出：那条 Warn 恰恰是唯一
// 没被断言的东西。BotAPI 嵌入的是 log.Log **接口**，所以可以换成记录型实现。

// recordingLog 记录 Warn 的 msg **与 fields**。
//
// 捕获 fields 是载荷性的，不是完整性洁癖：生产代码从不把原始值放进 msg，泄露风险
// 全在 fields 里。上一版只记 msg，于是把 zap.Int("rejectedLen", …) 换成
// zap.String("value", *req.AgentHosting) 会把任意 caller-controlled 字节送进日志管道，
// 而那条「不泄露」的断言照样绿（PR #837 round 4 P2-1）。
type recordingLog struct {
	warns []string
}

// renderFields 把 fields 编码成可断言的文本。用 zap 自己的 encoder，
// 这样断言看到的就是真正会进日志的内容，而不是我们对它的猜测。
func renderFields(fields []zap.Field) string {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	return fmt.Sprint(enc.Fields)
}

func (r *recordingLog) Info(msg string, fields ...zap.Field)  {}
func (r *recordingLog) Debug(msg string, fields ...zap.Field) {}
func (r *recordingLog) Error(msg string, fields ...zap.Field) {}
func (r *recordingLog) Warn(msg string, fields ...zap.Field) {
	r.warns = append(r.warns, msg+" | "+renderFields(fields))
}

// TestApplyAgentReportWarnsOnMalformedHostingWithoutLeakingTheValue —— 形状非法时
// 必须发 Warn，且**不得**把原始值写进日志（msg 与 fields 都不行）。
//
// 后半条是安全要求：值是 caller-controlled 的，进日志等于把任意字节（含换行、
// 控制字符）注入日志管道。只记长度。
//
// **杀掉的变异**：把 zap.Int("rejectedLen", …) 换成
// zap.String("value", *req.AgentHosting)。recordingLog 现在会渲染 fields，所以该变异
// 会让本测试红 —— 上一版只记 msg，抓不住它。
func TestApplyAgentReportWarnsOnMalformedHostingWithoutLeakingTheValue(t *testing.T) {
	rec := &recordingLog{}
	d, mock, closer := newAgentReportDB(t)
	defer closer()
	// 非法 hosting + 一个合法字段 → 仍会发一条 UPDATE（写 platform）。
	mock.ExpectExec("UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
	ba := &BotAPI{db: d, Log: rec}

	const secret = "<script>alert('pwn')</script>"
	ba.applyAgentReport("bot_x", &BotRegisterReq{
		AgentPlatform: strptr("OpenClaw"),
		AgentHosting:  strptr(secret),
	}, agentReport{})

	require.Len(t, rec.warns, 1, "形状非法必须留下一条 Warn —— 否则上报方无从得知自己白报了")
	assert.Contains(t, rec.warns[0], "agent_hosting")
	for _, w := range rec.warns {
		assert.NotContains(t, w, secret,
			"原始值是 caller-controlled 的，绝不能进日志（只记 rejectedLen）")
		assert.NotContains(t, w, "script")
	}
}

// TestApplyAgentReportSilentOnValidHosting —— 合法上报不该产生噪音。
func TestApplyAgentReportSilentOnValidHosting(t *testing.T) {
	rec := &recordingLog{}
	d, mock, closer := newAgentReportDB(t)
	defer closer()
	mock.ExpectExec("UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
	ba := &BotAPI{db: d, Log: rec}

	ba.applyAgentReport("bot_x", &BotRegisterReq{AgentHosting: strptr("vendor_hosted")}, agentReport{})
	assert.Empty(t, rec.warns, "合法上报不应产生 Warn")
}

// TestApplyAgentReportMalformedHostingLeavesColumnOutOfStatement —— 与 botfather 包
// 里那条端到端测试互补：这里直接确认被拒的 hosting **不进语句**（而不是以空串
// 进去覆盖已存值）。
func TestApplyAgentReportMalformedHostingLeavesColumnOutOfStatement(t *testing.T) {
	var got string
	d, mock, closer := newAgentReportDBCapturing(t, &got)
	defer closer()
	ba := &BotAPI{db: d, Log: &recordingLog{}}

	ba.applyAgentReport("bot_x", &BotRegisterReq{
		AgentPlatform: strptr("OpenClaw"),
		AgentHosting:  strptr("self-hosted"), // 连字符，被拒
	}, agentReport{})
	require.NoError(t, mock.ExpectationsWereMet())

	assert.Contains(t, got, "agent_platform", "同一请求里的合法字段仍应写入\nSQL: %s", got)
	assert.NotContains(t, got, "agent_hosting",
		"被拒的 hosting 必须整列缺席，不能以空串覆盖已存的合法值\nSQL: %s", got)
	assert.NotContains(t, got, "agent_reported_hosting_at",
		"没有有效的 hosting 上报，就不该盖 hosting 时间戳\nSQL: %s", got)
}

// ---------- 稀疏写入的结构守卫 ----------
//
// **杀掉的变异**：在 registerUserBot / applyAgentReport 里把缺席字段解析成刚读到的
// robot 行的值（read-merge-write），也就是前两轮被否决的实现。
//
// 为什么必须靠源码守卫而不是行为断言：reviewer 实测证明三条端到端测试**杀不掉**这个
// 变异（PR #837 round 4 P1(b)）—— 带外 UPDATE 落在下一次 register 之前，合并实现读到
// 的已经是新值，再写回去与稀疏写入无从区分；而 sqlmock 那层直接调
// updateRobotAgentInfo，在合并逻辑所在层**之下**。真正的交错窗口在
// queryRobotByBotToken 与 UPDATE 之间，行为测试无法确定性地插进去。
//
// 所以改用两条结构性事实：
//   1. applyAgentReport 只接受 robotID，签名上拿不到 robot 行 —— 想合并必须先改签名。
//   2. 函数体内不出现对 robot 行 agent_* 字段的读取。
// 这两条一起让"重新引入合并"无法悄悄发生：它会先撞到这条测试。

// TestApplyAgentReportCannotReadTheRobotRow —— 签名与函数体都不得触及 robot 行。
func TestApplyAgentReportCannotReadTheRobotRow(t *testing.T) {
	raw, err := os.ReadFile("register.go")
	require.NoError(t, err)
	src := string(raw)

	// (1) 签名不得接收 *robotModel：拿不到整行就无法回写"读到的值"。
	//
	// 允许它收一个 `stored agentReport` 快照 —— 那只喂给 changed() 做跳过比较，
	// 不匹配时写入的是调用方的值。禁止的是把整个 robot 行递进来（那会让"顺手取一个
	// 字段填补缺席"变成一行代码的距离）。
	assert.Regexp(t, regexp.MustCompile(
		`func \(ba \*BotAPI\) applyAgentReport\(robotID string, req \*BotRegisterReq(, stored agentReport)?\)`), src,
		"applyAgentReport 的第一个参数必须是 robotID 而非 robot 行 —— 传整行是 "+
			"read-merge-write 的入口，会重新引入两轮前被否决的丢更新")
	assert.NotContains(t, src, "func (ba *BotAPI) applyAgentReport(robot *robotModel",
		"applyAgentReport 不得接收 *robotModel")

	// (2) 函数体内不读 robot 行的 agent_* 字段。
	start := strings.Index(src, "func (ba *BotAPI) applyAgentReport(")
	require.NotEqual(t, -1, start)
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end != -1 {
		body = body[:end]
	}
	for _, forbidden := range []string{
		"robot.AgentPlatform", "robot.AgentVersion", "robot.PluginVersion", "robot.AgentHosting",
	} {
		assert.NotContains(t, body, forbidden,
			"applyAgentReport 不得读取 robot 行的 %s 来填补缺席字段：缺席必须在 SQL 层面缺席，"+
				"回写「刚读到的值」在并发 register 下会丢更新", forbidden)
	}
}

// TestRegisterUserBotPassesStoredOnlyAsASkipSnapshot —— 调用点可以把 robot 行的值
// 当作**跳过比较**的快照传下去，但不得把它们塞进请求体。
//
// **杀掉的变异**：在 registerUserBot 里写
// `if req.AgentVersion == nil { v := robot.AgentVersion; req.AgentVersion = &v }`
// —— 也就是把 read-merge-write 挪到调用点。已实测该变异会让本测试红。
//
// 区分点是**赋值目标**，不是"是否读了 robot 行"：
//   - 合法：`agentReport{Platform: &robot.AgentPlatform, ...}` 作为独立的 stored 快照
//     传参。它只用于 changed() 比较，不匹配时写入的是**调用方的值**，永远不是它。
//   - 禁止：把 robot 行的值写回 `req.Agent*` 任一字段。那样 applyAgentReport 收到的
//     就是「看起来是上报」的旧值，缺席字段又变成了会被回写的字段。
func TestRegisterUserBotPassesStoredOnlyAsASkipSnapshot(t *testing.T) {
	raw, err := os.ReadFile("register.go")
	require.NoError(t, err)
	src := string(raw)

	start := strings.Index(src, "func (ba *BotAPI) registerUserBot(")
	require.NotEqual(t, -1, start)
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end != -1 {
		body = body[:end]
	}

	// 禁止：把 robot 行的值赋回请求体的任一 agent 字段。
	for _, field := range []string{"AgentPlatform", "AgentVersion", "PluginVersion", "AgentHosting"} {
		assert.NotContains(t, body, "req."+field+" = ",
			"不得把值赋回 req.%s —— 用 robot 行的值填补缺席字段就是 read-merge-write，"+
				"在并发 register 下丢更新（A 读旧 → B 写新 → A 把旧值写回）", field)
	}

	// 允许且期望：stored 快照作为独立实参传入。这条同时防止有人"顺手"把 skip 优化删掉，
	// 那会让每次重连都发一条无可观察效果的空写（PR #837 round 4 P2-2）。
	assert.Regexp(t,
		regexp.MustCompile(`applyAgentReport\([^)]*&req,\s*agentReport\{`),
		strings.ReplaceAll(body, "\n", " "),
		"stored 快照应作为独立实参传给 applyAgentReport，而不是并入 req")
}

// TestRegisterAppBotWarnIsTheDeliverable —— App Bot 分支解析 body 的**唯一**产出
// 就是这条 Warn，所以它必须被断言。
//
// brief 曾声称"App Bot 上报时的 Warn 被断言"，而实际没有任何测试碰它 ——
// 删掉 register.go 里那行 Warn，整个 suite 照样绿（PR #837 round 4 Spec item 3）。
//
// **杀掉的变异**：删除 registerAppBot 里的 ba.Warn(...)。
//
// 用源码断言而非行为断言：那条 Warn 发生在 registerAppBot 内部，而该函数需要
// app_bot 行 + IM token 往返才能走到，在本包（无 botfather 迁移）跑不起来。
// 断言"这一行还在、且带的是 uid 而非原始上报值"是能确定性做到的部分。
func TestRegisterAppBotWarnIsTheDeliverable(t *testing.T) {
	raw, err := os.ReadFile("register.go")
	require.NoError(t, err)
	src := string(raw)

	start := strings.Index(src, "func (ba *BotAPI) registerAppBot(")
	require.NotEqual(t, -1, start)
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end != -1 {
		body = body[:end]
	}

	assert.Contains(t, body, "readAgentReport(c)",
		"registerAppBot 必须解析 body —— 否则上报方永远发现不了自己白报了")
	assert.Contains(t, body, "ba.Warn(",
		"registerAppBot 解析 body 的唯一产出就是这条 Warn；删掉它，解析就成了纯开销")
	assert.NotContains(t, body, "updateRobotAgentInfo",
		"App Bot 的上报绝不能写进 robot 表（app_bot 没有 agent_* 列）")
}
