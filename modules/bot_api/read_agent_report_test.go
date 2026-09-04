package bot_api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readAgentReport 的 body 解码边界。
//
// 这些用例只碰 readAgentReport 本身：它不访问 DB、不用 wkhttp.Context 的 logger，
// 所以能在 bot_api 包内直接驱动，无需 botfather 的迁移（agent_* 列不在本包）。
// 端到端那侧的覆盖在 modules/botfather/agent_hosting_test.go。

// newDecodeCtx 造一个只够 readAgentReport 用的 context：它只读 c.Request.Body
// 和 c.Writer。wkhttp.Context 的 lg/wk 是非导出字段，留零值即可 —— 一旦
// readAgentReport 开始记日志，这里会 panic，那正是"它不该记日志"的信号。
func newDecodeCtx(body io.ReadCloser) *wkhttp.Context {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/bot/register", nil)
	ginCtx.Request.Body = body
	return &wkhttp.Context{Context: ginCtx}
}

// stalledBody 发完 payload 后永久阻塞，等价于线上这种客户端：
//
//	POST /v1/bot/register HTTP/1.1
//	Transfer-Encoding: chunked
//
//	1f
//	{"agent_hosting":"self_hosted"}
//	          <- 到此不再发送，永不发 0 长度结束块
//
// Read 挂住而不返回 EOF，正是挂在一个没有 read deadline 的 socket 上的样子。
type stalledBody struct {
	payload []byte
	off     int
	release chan struct{} // 只由 t.Cleanup 关闭，用来回收测试 goroutine
}

func (s *stalledBody) Read(p []byte) (int, error) {
	if s.off < len(s.payload) {
		n := copy(p, s.payload[s.off:])
		s.off += n
		return n, nil
	}
	<-s.release
	return 0, io.EOF
}

func (s *stalledBody) Close() error { return nil }

// TestReadAgentReportDoesNotBlockOnStalledBody —— 停滞的请求体不得挂住 handler。
//
// PR #837 的 P1。曾经有一轮在 Decode 之后加了 dec.Token() 来顺带拒绝"合法对象 +
// 尾随垃圾"。Decode 解析完一个完整值就返回，不需要 EOF；Token 为了区分"还有
// token"和"输入结束"必须再读至少一个字节。MaxBytesReader 限的是字节数不是时间，
// 而这条路由由零值 http.Server 承载（gin 的 r.Run()，ReadTimeout=0），于是
// Token 会永久阻塞在 socket 上，占住一个 goroutine 和一条连接；User Bot 分支上
// 这个挂起还发生在 UpdateIMToken 已经改过状态之后。register 是 bot 掉线后唯一的
// 自愈通道（#696），不能有任何挂住它的路径。
//
// 这个用例确定性地杀掉"把 dec.Token() 加回来"这个变异：加回去它就超时。
// io.ReadAll 替代 Decode 也一样超时（ReadAll 同样循环到 EOF），所以这条也被挡住。
func TestReadAgentReportDoesNotBlockOnStalledBody(t *testing.T) {
	body := &stalledBody{
		payload: []byte(`{"agent_hosting":"self_hosted"}`),
		release: make(chan struct{}),
	}
	// 超时路径下 goroutine 还挂在 <-release 上，关掉它让其退出。
	t.Cleanup(func() { close(body.release) })

	done := make(chan BotRegisterReq, 1)
	go func() { done <- readAgentReport(newDecodeCtx(body)) }()

	select {
	case got := <-done:
		require.NotNil(t, got.AgentHosting,
			"完整的 JSON 对象已经收齐，body 未终止不影响它被采纳")
		assert.Equal(t, "self_hosted", *got.AgentHosting)
	case <-time.After(3 * time.Second):
		t.Fatal("readAgentReport 在未终止的 body 上挂住了 —— register 变成可挂死的端点（PR #837 P1）")
	}
}

// TestReadAgentReportNilBodyIsNotAPanic —— nil body 不得 panic。
//
// net/http 保证服务端请求的 Body 非 nil，所以线上走不到；但 handler 也会被
// 进程内驱动，http.NewRequest(..., nil) 留下的就是 nil Body（本包
// ratelimit_integration_test.go 的 botPost 正是如此）。gin 的 jsonBinding.Bind
// 对此有早返回，换成 MaxBytesReader 后就没有了：它会包住一个 nil ReadCloser，
// 第一次 Read 就是 nil 指针解引用。
//
// 杀掉的变异：删掉 readAgentReport 里的 `if c.Request.Body == nil` 早返回。
func TestReadAgentReportNilBodyIsNotAPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/bot/register", nil)
	ginCtx.Request.Body = nil // http.NewRequest(..., nil) 之后的实际状态

	require.NotPanics(t, func() {
		assert.Equal(t, BotRegisterReq{}, readAgentReport(&wkhttp.Context{Context: ginCtx}))
	}, "nil body 必须当作「什么都没上报」，而不是打挂 handler")
}

// TestReadAgentReportTypeErrorAdoptsNothing —— 类型错误的 body 一个字段都不采纳。
//
// json.Decoder 会先填好已解析的字段再返回类型错误，所以"解进结果里再忽略错误"
// 等于采纳一个前缀：下面这个 body 会存下 platform、丢掉后面，且没有任何诊断。
// 半更新的列比不更新更糟 —— 它看起来像一次成功上报。
//
// 杀掉的变异：把 staged 临时变量去掉、直接解进返回值并忽略 err。
func TestReadAgentReportTypeErrorAdoptsNothing(t *testing.T) {
	got := readAgentReport(newDecodeCtx(
		io.NopCloser(strings.NewReader(`{"agent_platform":"OpenClaw","agent_version":12345}`))))
	assert.Equal(t, BotRegisterReq{}, got,
		"类型错误的 body 一个字段都不该被采纳 —— 半更新看起来像成功上报")
}

// TestReadAgentReportTrailingGarbageIsIgnoredNotRejected —— 尾随垃圾被忽略，
// 前面那个合法对象照常采纳。
//
// 这是 P1 修复后有意的语义，不是遗漏：拒绝尾随垃圾要求判定输入结束，而判定输入
// 结束要求在未终止的 body 上多读一个字节（见
// TestReadAgentReportDoesNotBlockOnStalledBody）。忽略一个畸形客户端多发的尾巴，
// 比让 bot 的自愈端点可被挂死代价小得多。真正要保的"全有或全无"由上面那条
// 类型错误用例覆盖，它不依赖 EOF 判定。
//
// 这条用例的作用是把这个取舍钉住：如果有人为了拒绝尾随垃圾把 EOF 检查加回来，
// 这里会红，同时 stalled-body 那条会超时 —— 两处一起说明代价。
func TestReadAgentReportTrailingGarbageIsIgnoredNotRejected(t *testing.T) {
	got := readAgentReport(newDecodeCtx(
		io.NopCloser(strings.NewReader(`{"agent_platform":"OpenClaw"} trailing`))))
	require.NotNil(t, got.AgentPlatform)
	assert.Equal(t, "OpenClaw", *got.AgentPlatform,
		"尾随垃圾之前的完整对象照常采纳；拒绝它的代价是端点可挂死（PR #837 P1）")
}
