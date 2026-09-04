package bot_api

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"regexp"
	"strconv"
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
			// 名字按**本层**的输入说话：到 updateRobotAgentInfo 时 Hosting=&"" 只可能
			// 来自 none 这个撤回 slug（线上 "" 是「不变」，见 normalizeAgentHosting）。
			// 别把它读成线上语义 —— round 4 换掉的正是那个契约。
			name:    "归一化后 Hosting=&\"\"（none 撤回）：列在，时间戳也在",
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
			// **这一行是这张表存在的理由**，而它此前一直缺席（PR #837 round 5 🔴2）：
			// 断言 legacy 列缺席的用例全都跑在 stored 为空的前提上，于是
			// 「stored 有值 + 字段缺席」这个唯一能暴露 read-merge-write 的组合从未被构造。
			//
			// **杀掉的变异**：在 updateRobotAgentInfo 里加
			//   else if report.Platform == nil && stored.Platform != nil {
			//       set["agent_platform"] = *stored.Platform
			//   }
			// （对 Version / Plugin / Hosting 同形），也就是 round 2 的丢更新。
			// 没有这一行，那个变异能穿过整个 bot_api 套件。
			name:   "stored 三个 legacy 都有值、只报 hosting：三列必须缺席（丢更新守卫）",
			report: agentReport{Hosting: strptr("octo_hosted")},
			stored: agentReport{
				Platform: strptr("OpenClaw"), Version: strptr("1.2.3"), Plugin: strptr("9.9.9"),
			},
			present: []string{"agent_hosting", "agent_reported_hosting_at"},
			absent:  []string{"agent_platform", "agent_version", "plugin_version"},
		},
		{
			// 反向：stored 有 hosting、只报版本 —— hosting 与其时间戳都必须缺席。
			// 少了它，「把 stored.Hosting 回写」的变异就只在端到端层可见（而端到端层
			// 已被证明区分不了两种实现）。
			name:   "stored 有 hosting、只报版本：hosting 与时间戳必须缺席",
			report: agentReport{Version: strptr("2.0.0")},
			stored: agentReport{
				Version: strptr("1.0.0"), Hosting: strptr("self_hosted"),
			},
			present: []string{"agent_version"},
			absent:  []string{"agent_hosting", "agent_reported_hosting_at", "agent_platform", "plugin_version"},
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

			var stmts []string
			mock.ExpectExec("UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
			require.NoError(t, d.updateRobotAgentInfo("bot_x", tc.report, tc.stored))
			require.NoError(t, mock.ExpectationsWereMet())

			// sqlmock 不直接回吐 SQL 文本，改用 QueryMatcher 捕获：重跑一次并抓语句。
			d2, mock2, closer2 := newAgentReportDBCapturing(t, &stmts)
			defer closer2()
			require.NoError(t, d2.updateRobotAgentInfo("bot_x", tc.report, tc.stored))
			require.NoError(t, mock2.ExpectationsWereMet())

			// 拼接后再断言：absent 的语义是"任何一条语句里都不出现"。
			got := strings.Join(stmts, "\n")
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

// newAgentReportDBCapturing 用自定义 QueryMatcher 抓取实际发出的**全部** SQL 文本。
//
// sink 累积而不是覆盖：今天只发一条语句，赋值也够；但已知限制（超长 legacy 字段
// 连坐整组）的修法就是把 UPDATE 拆开，那之后覆盖式赋值只会留下最后一条，
// present/absent 断言就在悄悄地只检查一个片段（PR #837 round 7 🔵）。
func newAgentReportDBCapturing(t *testing.T, sink *[]string) (*botAPIDB, sqlmock.Sqlmock, func()) {
	t.Helper()
	// 捕获的同时**仍然校验** —— 无条件 return nil 会让 ExpectExec("UPDATE") 形同虚设
	// （round 5 🔵）：那时任何语句都被接受，mock 自己的守卫是关着的。
	matcher := sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
		*sink = append(*sink, actualSQL)
		if !regexp.MustCompile(expectedSQL).MatchString(actualSQL) {
			return fmt.Errorf("语句与预期不符\n预期正则: %s\n实际: %s", expectedSQL, actualSQL)
		}
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
	var stmts []string
	d, mock, closer := newAgentReportDBCapturing(t, &stmts)
	defer closer()

	require.NoError(t, d.updateRobotAgentInfo("bot_x", agentReport{Hosting: strptr("self_hosted")}, agentReport{}))
	require.NoError(t, mock.ExpectationsWereMet())

	got := strings.Join(stmts, "\n")
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
	var stmts []string
	d, mock, closer := newAgentReportDBCapturing(t, &stmts)
	defer closer()
	ba := &BotAPI{db: d, Log: &recordingLog{}}

	ba.applyAgentReport("bot_x", &BotRegisterReq{
		AgentPlatform: strptr("OpenClaw"),
		AgentHosting:  strptr("self-hosted"), // 连字符，被拒
	}, agentReport{})
	require.NoError(t, mock.ExpectationsWereMet())

	got := strings.Join(stmts, "\n")
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
// 注意分工（round 5 🔴2 修正）：**db 层**那个变异形态
// （`else if report.X == nil && stored.X != nil { set[...] = *stored.X }`）不由这两条
// 守卫承担，而由 TestUpdateRobotAgentInfoOmitsUnreportedColumns 里
// 「stored 有值 + 字段缺席」那两行用例杀掉 —— 已实测。这两条守卫管的是
// **调用层**：不让 stored 的值流进 req/report。
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

	// (2) 函数体内不得把 stored 的值搬进 report/req —— stored 只能进 changed() 比较。
	//
	// reviewer 给出的绕过路径（round 5 🔴2(b)）：在快照里加 `Hosting: &robot.AgentHosting`，
	// 再在本函数里补 `if req.AgentHosting == nil { req.AgentHosting = stored.Hosting }`，
	// 于是版本-only 重连会写回陈旧形态**并推进时间戳**，丢掉并发的撤回 —— 而此前两条
	// 守卫全绿。legacy 那一支的同形变异是行为等价的（changed() 拿值和自己比、跳过），
	// 所以只有 hosting 这一支有害，但两支都禁掉更简单、也不留下"哪支可以"的判断题。
	appStart := strings.Index(src, "func (ba *BotAPI) applyAgentReport(")
	require.NotEqual(t, -1, appStart)
	appBody := src[appStart:]
	if end := strings.Index(appBody, "\n}\n"); end != -1 {
		appBody = appBody[:end]
	}
	for _, field := range []string{"AgentPlatform", "AgentVersion", "PluginVersion", "AgentHosting"} {
		assert.NotContains(t, appBody, "req."+field+" = ",
			"applyAgentReport 不得把值赋回 req.%s —— stored 只能喂给 changed() 比较，"+
				"搬进请求体就是 read-merge-write（hosting 那一支会写回陈旧形态并推进时间戳，"+
				"丢掉并发撤回）", field)
	}
	for _, f := range []string{"stored.Platform", "stored.Version", "stored.Plugin", "stored.Hosting"} {
		assert.NotContains(t, appBody, "report."+strings.TrimPrefix(f, "stored.")+" = "+f,
			"不得把 %s 直接赋给 report 的同名字段 —— 那是把 stored 当成上报值", f)
	}

	// (3) 函数体内不读 robot 行的 agent_* 字段。
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
	flat := strings.Join(strings.Fields(body), " ")
	assert.Regexp(t,
		regexp.MustCompile(`applyAgentReport\([^)]*&req, agentReport\{`), flat,
		"stored 快照应作为独立实参传给 applyAgentReport，而不是并入 req")

	// 快照**只含三个 legacy 字段**：hosting 不进 skip 比较，因为撤回/再确认都是一次
	// 真实上报、时间戳必须推进；把 Hosting 放进快照是 round 5 🔴2(b) 那条绕过路径的
	// 前半步，在这里就掐掉。
	snapStart := strings.Index(flat, "applyAgentReport(")
	require.NotEqual(t, -1, snapStart)
	snap := flat[snapStart:]
	if end := strings.Index(snap, "})"); end != -1 {
		snap = snap[:end]
	}
	assert.NotContains(t, snap, "Hosting:",
		"stored 快照不得包含 Hosting —— hosting 没有 skip-if-unchanged（撤回也是上报），"+
			"把它放进快照只会为 read-merge-write 开路")
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
	fn, fset := parseFuncDecl(t, "register.go", "registerAppBot")
	body := renderNode(t, fset, fn.Body)

	assert.Contains(t, body, "readAgentReport(c)",
		"registerAppBot 必须解析 body —— 否则「有 bot 在上报没人存的 telemetry」这件事对运维不可见")
	assert.NotContains(t, body, "updateRobotAgentInfo",
		"App Bot 的上报绝不能写进 robot 表（app_bot 没有 agent_* 列）")

	// Warn 这一条走 AST 而非子串：要断言的是「带 uid、且不带客户端上报的原始值」，
	// 这是调用实参的性质，子串匹配 "ba.Warn(" 表达不了 —— PR #837 round 7 量到
	// 把它换成 ba.Warn("ignored", zap.String("agent_hosting", *req.AgentHosting))
	// （丢掉 uid 又把调用方可控的值写进日志）照样能过。
	warns := warnCallsIn(t, fset, fn)
	require.NotEmpty(t, warns,
		"registerAppBot 解析 body 的唯一产出就是给**运维**看的 Warn（客户端只拿到 200）；删掉它，解析就成了纯开销")

	var withUID int
	for _, w := range warns {
		if w.hasZapField("uid") {
			withUID++
		}
		for _, f := range w.fieldKeys {
			assert.NotContains(t, w.args, "req.Agent",
				"这条 Warn 不得把客户端上报的原始值写进日志（字段 %q）—— 只记 uid，够运维定位是哪个 bot 在上报", f)
			assert.NotContains(t, w.args, "req.Plugin",
				"这条 Warn 不得把客户端上报的原始值写进日志（字段 %q）", f)
		}
	}
	assert.NotZero(t, withUID,
		"至少一条 Warn 要带 zap.String(\"uid\", ...) —— 不带 uid 的告警运维无法定位是哪个 App Bot 在上报")
}

// warnCall 是 registerAppBot 里一次 ba.Warn 调用的 AST 摘要。
type warnCall struct {
	args      string   // 全部实参的源码文本，用于"不得出现某东西"这类断言
	fieldKeys []string // zap.Xxx("key", ...) 里的 key 字面量
}

func (w warnCall) hasZapField(key string) bool {
	for _, k := range w.fieldKeys {
		if k == key {
			return true
		}
	}
	return false
}

// parseFuncDecl 在一个 .go 文件里按**函数名**定位声明。
//
// 刻意不按 "func (ba *BotAPI) registerAppBot(" 这样的源码字串定位：接收者改名
// （ba → b）就会让定位失败，而那是纯粹的重构，守卫不该因此变红 —— PR #837
// round 7 量到过一次。
func parseFuncDecl(t *testing.T, file, name string) (*ast.FuncDecl, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	require.NoError(t, err)
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd, fset
		}
	}
	t.Fatalf("%s 里找不到函数 %s", file, name)
	return nil, nil
}

// renderNode 把 AST 节点打回源码文本，供"函数体里必须/不得出现某调用"这类断言使用。
// 经过 printer 归一化，所以不受 CRLF checkout 和实参间距影响。
func renderNode(t *testing.T, fset *token.FileSet, n ast.Node) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, printer.Fprint(&buf, fset, n))
	return buf.String()
}

// warnCallsIn 取出一个函数体内所有 .Warn(...) 调用。
//
// 只匹配选择子名 Warn，不看接收者，所以接收者改名不影响它。
func warnCallsIn(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl) []warnCall {
	t.Helper()
	var out []warnCall
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Warn" {
			return true
		}
		w := warnCall{}
		var buf bytes.Buffer
		for _, a := range call.Args {
			require.NoError(t, printer.Fprint(&buf, fset, a))
			buf.WriteByte('\n')
			// zap.Xxx("key", ...) —— 取第一个实参的字符串字面量作为字段名。
			inner, ok := a.(*ast.CallExpr)
			if !ok || len(inner.Args) == 0 {
				continue
			}
			lit, ok := inner.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if key, err := strconv.Unquote(lit.Value); err == nil {
				w.fieldKeys = append(w.fieldKeys, key)
			}
		}
		w.args = buf.String()
		out = append(out, w)
		return true
	})
	return out
}
