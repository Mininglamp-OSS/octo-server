package user

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	common2 "github.com/Mininglamp-OSS/octo-server/modules/common"
)

// stubCommonService 只为覆盖 sentWelcomeMsg 的 GetAppConfig 失败分支，
// 其余方法不会被调用。
type stubCommonService struct {
	resp *common2.AppConfigResp
	err  error
}

func (s *stubCommonService) GetAppConfig() (*common2.AppConfigResp, error) {
	return s.resp, s.err
}
func (s *stubCommonService) GetShortno() (string, error)                   { return "", nil }
func (s *stubCommonService) SetShortnoUsed(shortno, business string) error { return nil }

// TestSentWelcomeMsgSurvivesAppConfigFailure 锁住空指针回归。
//
// sentWelcomeMsg 只以 `go u.sentWelcomeMsg(...)` 调用，裸 goroutine 的 panic 逃不出
// gin 的 recovery —— 一次解引用带走的是整个进程，不是一个请求。而 GetAppConfig 在
// app_config 查询失败时返回 (nil, err)，所以"只打日志不 return"会在数据库抖动期间
// 被每一次成功登录踩到，正好在数据库已经不健康时把服务打成崩溃循环。
//
// 本 PR 把 finishSuccessfulLogin 的调用点从 2 个扩到 9 个（新增扫码登录、手机验证码
// 登录两条高频移动端路径），所以这条路径的触发面比修复前大得多。
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
			u := &User{Log: log.NewTLog("user-test"), commonService: tc.svc}
			// 不 panic 即通过：函数必须在解引用 appconfig 之前返回。
			// 若把守卫去掉，这里会 panic 而不是 fail，所以用 defer 明确断言。
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("sentWelcomeMsg 在配置读取失败时 panic 了：%v", r)
				}
			}()
			u.sentWelcomeMsg("1.2.3.4", "u-1", nil)
		})
	}
}
