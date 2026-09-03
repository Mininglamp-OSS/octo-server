package bot_api

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
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
			name:    "全都报：五列俱在",
			report:  agentReport{Platform: strptr("OpenClaw"), Version: strptr("1"), Plugin: strptr("2"), Hosting: strptr("octo_hosted")},
			present: []string{"agent_platform", "agent_version", "plugin_version", "agent_hosting", "agent_reported_hosting_at"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, mock, closer := newAgentReportDB(t)
			defer closer()

			var got string
			mock.ExpectExec("UPDATE").WillReturnResult(sqlmock.NewResult(0, 1))
			require.NoError(t, d.updateRobotAgentInfo("bot_x", tc.report))
			require.NoError(t, mock.ExpectationsWereMet())

			// sqlmock 不直接回吐 SQL 文本，改用 QueryMatcher 捕获：重跑一次并抓语句。
			d2, mock2, closer2 := newAgentReportDBCapturing(t, &got)
			defer closer2()
			require.NoError(t, d2.updateRobotAgentInfo("bot_x", tc.report))
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

	require.NoError(t, d.updateRobotAgentInfo("bot_x", agentReport{Hosting: strptr("self_hosted")}))
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
	require.NoError(t, d.updateRobotAgentInfo("bot_x", agentReport{}))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------- Warn 断言 ----------
//
// registerAppBot 解析 body 的**唯一**理由就是发这条 Warn（「不发的话上报方永远
// 发现不了自己白报了」）。PR #837 的两位 reviewer 都指出：那条 Warn 恰恰是唯一
// 没被断言的东西。BotAPI 嵌入的是 log.Log **接口**，所以可以换成记录型实现。

// recordingLog 记录 Warn 调用，其余级别丢弃。
type recordingLog struct {
	warns []string
}

func (r *recordingLog) Info(msg string, fields ...zap.Field)  {}
func (r *recordingLog) Debug(msg string, fields ...zap.Field) {}
func (r *recordingLog) Error(msg string, fields ...zap.Field) {}
func (r *recordingLog) Warn(msg string, fields ...zap.Field) {
	r.warns = append(r.warns, msg)
}

// TestApplyAgentReportWarnsOnMalformedHostingWithoutLeakingTheValue —— 形状非法时
// 必须发 Warn，且**不得**把原始值写进日志。
//
// 后半条是安全要求：值是 caller-controlled 的，进日志等于把任意字节（含换行、
// 控制字符）注入日志管道。只记长度。
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
	})

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

	ba.applyAgentReport("bot_x", &BotRegisterReq{AgentHosting: strptr("vendor_hosted")})
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
	})
	require.NoError(t, mock.ExpectationsWereMet())

	assert.Contains(t, got, "agent_platform", "同一请求里的合法字段仍应写入\nSQL: %s", got)
	assert.NotContains(t, got, "agent_hosting",
		"被拒的 hosting 必须整列缺席，不能以空串覆盖已存的合法值\nSQL: %s", got)
	assert.NotContains(t, got, "agent_reported_hosting_at",
		"没有有效的 hosting 上报，就不该盖 hosting 时间戳\nSQL: %s", got)
}
