package user

import (
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	common2 "github.com/Mininglamp-OSS/octo-server/modules/common"
)

// stubCommonService 只为覆盖 sentWelcomeMsg 的提前返回分支，其余方法不会被调用。
type stubCommonService struct {
	resp *common2.AppConfigResp
	err  error
}

func (s *stubCommonService) GetAppConfig() (*common2.AppConfigResp, error) {
	return s.resp, s.err
}
func (s *stubCommonService) GetShortno() (string, error)                   { return "", nil }
func (s *stubCommonService) SetShortnoUsed(shortno, business string) error { return nil }

// assertSentWelcomeMsgReturnsEarly 断言 sentWelcomeMsg 在走到发送分支之前就返回。
//
// 两条独立证据，缺一条都可能给出假阳性：
//   - u.ctx 是刻意留 nil 的。越过提前返回就会走到 u.ctx.GetConfig()，在测试里必然
//     panic，所以"没 panic"是提前返回的正向证据，而不是弱断言。
//   - 发送分支前有一段 500ms sleep。elapsed 远小于它，说明确实没往下走 —— 这条不
//     依赖 panic 具体发生在哪一行，即使将来 nil 解引用的位置变了也仍然成立。
func assertSentWelcomeMsgReturnsEarly(t *testing.T, svc *stubCommonService) {
	t.Helper()
	u := &User{Log: log.NewTLog("user-test"), commonService: svc}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("sentWelcomeMsg 应提前返回，实际继续执行并 panic：%v", r)
		}
	}()
	start := time.Now()
	u.sentWelcomeMsg("1.2.3.4", "u-1", nil)
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("sentWelcomeMsg 越过了发送前的 500ms sleep，说明没有提前返回（耗时 %v）", elapsed)
	}
}

// TestSentWelcomeMsgSurvivesAppConfigFailure 锁住空指针回归。
//
// sentWelcomeMsg 只以 `go u.sentWelcomeMsg(...)` 调用，裸 goroutine 的 panic 逃不出
// gin 的 recovery —— 一次解引用带走的是整个进程，不是一个请求。而 GetAppConfig 在
// app_config 查询失败时返回 (nil, err)，所以"只打日志不 return"会在数据库抖动期间
// 被每一次成功登录踩到，正好在数据库已经不健康时把服务打成崩溃循环。
//
// 两个用例覆盖 GetAppConfig 的两种"无配置"返回形状：(nil, err) 与 (nil, nil)。
// 后者不是假设 —— appConfigDB.query() 在 app_config 空表时就返回 (nil, nil)。
func TestSentWelcomeMsgSurvivesAppConfigFailure(t *testing.T) {
	cases := []struct {
		name string
		svc  *stubCommonService
	}{
		{name: "query error", svc: &stubCommonService{resp: nil, err: errors.New("boom")}},
		{name: "no row, no error", svc: &stubCommonService{resp: nil, err: nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSentWelcomeMsgReturnsEarly(t, tc.svc)
		})
	}
}

// TestSentWelcomeMsgRespectsWelcomeToggle 锁住 send_welcome_message_on 这个运维开关。
//
// 它是管理员可在后台读写的 app_config 字段（列默认 1），关掉之后就不该再推欢迎语 ——
// 消息体里带着上次登录的 IP 和时间，误发不只是噪音。
//
// 单独成一个测试而不是并进上面那个：两者都是"提前返回"，但契约不同，一个是空配置
// 守卫、一个是功能开关。本 PR 上一版把空指针守卫上提时，正是因为两个 return 长得像
// 而把开关连带吞掉了，导致九条登录路径重新开始推送。分开命名，失败时能直接看出坏的
// 是哪一个契约。
func TestSentWelcomeMsgRespectsWelcomeToggle(t *testing.T) {
	assertSentWelcomeMsgReturnsEarly(t, &stubCommonService{
		resp: &common2.AppConfigResp{SendWelcomeMessageOn: 0},
	})
}
