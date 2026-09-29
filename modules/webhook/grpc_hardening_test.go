package webhook

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// 这些测试不依赖 MySQL / Redis：监听器绑定在回环地址的空闲端口上，
// 只使用最小化的 config.Context。所有 token 均为合成值。

const testGRPCSentinelToken = "synthetic-grpc-token-5b2e9c"

type recordedLogEntry struct {
	level string
	msg   string
	text  string // msg + 编码后的全部字段
}

// recordingLog 实现 log.Log，记录所有日志用于断言。
type recordingLog struct {
	mu      sync.Mutex
	entries []recordedLogEntry
}

func (r *recordingLog) record(level, msg string, fields []zap.Field) {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, recordedLogEntry{level: level, msg: msg, text: fmt.Sprintf("%s %v", msg, enc.Fields)})
}

func (r *recordingLog) Info(msg string, fields ...zap.Field)  { r.record("info", msg, fields) }
func (r *recordingLog) Debug(msg string, fields ...zap.Field) { r.record("debug", msg, fields) }
func (r *recordingLog) Error(msg string, fields ...zap.Field) { r.record("error", msg, fields) }
func (r *recordingLog) Warn(msg string, fields ...zap.Field)  { r.record("warn", msg, fields) }

func (r *recordingLog) snapshot() []recordedLogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedLogEntry(nil), r.entries...)
}

func (r *recordingLog) levels() map[string]int {
	out := map[string]int{}
	for _, e := range r.snapshot() {
		out[e.level]++
	}
	return out
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// startTestWebhook 以给定环境变量启动 webhook gRPC 监听器。
func startTestWebhook(t *testing.T, token, required, addr string) (*Webhook, *config.Context, *recordingLog, error) {
	t.Helper()
	t.Setenv(grpcAuthTokenEnv, token)
	t.Setenv(grpcAuthRequiredEnv, required)
	cfg := config.New()
	cfg.GRPCAddr = addr
	ctx := config.NewContext(cfg)
	rec := &recordingLog{}
	w := &Webhook{Log: rec, ctx: ctx}
	err := w.Start()
	t.Cleanup(func() { _ = w.Stop() })
	return w, ctx, rec, err
}

// callSendWebhook 调用 SendWebhook；token 为空时不携带 auth_token。
func callSendWebhook(t *testing.T, addr, token, event string, data []byte) (*wkhook.EventResp, error) {
	t.Helper()
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, grpcAuthMetadataKey, token)
	}
	return wkhook.NewWebhookServiceClient(conn).SendWebhook(ctx, &wkhook.EventReq{Event: event, Data: data})
}

// noopEvent 不命中任何事件处理分支，handleEvent 直接返回成功。
const noopEvent = "test.noop"

func assertUnauthenticated(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestLoadGRPCAuthConfig(t *testing.T) {
	tests := []struct {
		name         string
		token        string
		required     string
		wantErr      bool
		wantRequired bool
	}{
		{name: "默认：无 token 不强制", token: "", required: ""},
		{name: "默认：有 token", token: "tok", required: ""},
		{name: "显式 false", token: "", required: "false"},
		{name: "强制且有 token", token: "tok", required: "true", wantRequired: true},
		{name: "强制值前后空白与大小写", token: "tok", required: " TRUE ", wantRequired: true},
		{name: "强制但无 token", token: "", required: "true", wantErr: true},
		{name: "非法布尔值", token: "tok", required: "yes", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{grpcAuthTokenEnv: tt.token, grpcAuthRequiredEnv: tt.required}
			cfg, err := loadGRPCAuthConfig(func(k string) string { return env[k] })
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.token, cfg.token)
			assert.Equal(t, tt.wantRequired, cfg.required)
		})
	}
}

func TestLoadGRPCAuthConfig_ErrorNeverContainsToken(t *testing.T) {
	env := map[string]string{grpcAuthTokenEnv: testGRPCSentinelToken, grpcAuthRequiredEnv: "maybe"}
	_, err := loadGRPCAuthConfig(func(k string) string { return env[k] })
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testGRPCSentinelToken)
}

func TestGRPCAuthInterceptor_EmptyExpectedTokenRejectsAll(t *testing.T) {
	interceptor := grpcAuthInterceptor("")
	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return nil, nil
	}
	contexts := map[string]context.Context{
		"无 metadata":       context.Background(),
		"空 auth_token":     metadata.NewIncomingContext(context.Background(), metadata.Pairs(grpcAuthMetadataKey, "")),
		"任意 auth_token":    metadata.NewIncomingContext(context.Background(), metadata.Pairs(grpcAuthMetadataKey, "anything")),
		"无 auth_token key": metadata.NewIncomingContext(context.Background(), metadata.Pairs("other", "x")),
	}
	for name, ctx := range contexts {
		t.Run(name, func(t *testing.T) {
			_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, handler)
			assertUnauthenticated(t, err)
		})
	}
	assert.False(t, called, "handler must not run when no token is configured")
}

// 源码断言：token 比较必须是常量时间比较。
func TestGRPCAuthInterceptor_UsesConstantTimeCompare(t *testing.T) {
	src, err := os.ReadFile("grpc_auth.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), "subtle.ConstantTimeCompare(")
	assert.NotContains(t, string(src), "!= expectedToken")
	assert.NotContains(t, string(src), "== expectedToken")
}

func TestIsLoopbackListenAddr(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:6979": true,
		"[::1]:6979":     true,
		"localhost:6979": true,
		"0.0.0.0:6979":   false,
		"[::]:6979":      false,
		":6979":          false,
		"10.0.0.8:6979":  false,
		"not-an-addr":    false,
	}
	for addr, want := range tests {
		assert.Equal(t, want, isLoopbackListenAddr(addr), addr)
	}
}

func TestWebhookGRPC_DefaultModeWithoutToken_AcceptsUnauthenticatedCall(t *testing.T) {
	addr := freeLoopbackAddr(t)
	_, _, rec, err := startTestWebhook(t, "", "", addr)
	require.NoError(t, err)

	resp, err := callSendWebhook(t, addr, "", noopEvent, nil)
	require.NoError(t, err, "default mode must stay backward compatible")
	assert.Equal(t, wkhook.EventStatus_Success, resp.Status)
	assert.Zero(t, rec.levels()["warn"], "loopback listener without token logs info, not warn")
}

func TestWebhookGRPC_DefaultModeWithToken_EnforcesToken(t *testing.T) {
	addr := freeLoopbackAddr(t)
	_, _, _, err := startTestWebhook(t, testGRPCSentinelToken, "", addr)
	require.NoError(t, err)

	_, err = callSendWebhook(t, addr, "", noopEvent, nil)
	assertUnauthenticated(t, err)

	resp, err := callSendWebhook(t, addr, testGRPCSentinelToken, noopEvent, nil)
	require.NoError(t, err)
	assert.Equal(t, wkhook.EventStatus_Success, resp.Status)
}

func TestWebhookGRPC_RequiredModeWithoutToken_RefusesToStart(t *testing.T) {
	addr := freeLoopbackAddr(t)
	_, _, _, err := startTestWebhook(t, "", "true", addr)
	require.Error(t, err)

	// 未监听端口：同一地址仍可被绑定。
	l, listenErr := net.Listen("tcp", addr)
	require.NoError(t, listenErr, "no port may be bound when start is refused")
	_ = l.Close()
}

func TestWebhookGRPC_InvalidRequiredValue_RefusesToStart(t *testing.T) {
	addr := freeLoopbackAddr(t)
	_, _, _, err := startTestWebhook(t, testGRPCSentinelToken, "on-ish", addr)
	require.Error(t, err)
}

func TestWebhookGRPC_RequiredModeWithToken_EnforcesToken(t *testing.T) {
	addr := freeLoopbackAddr(t)
	_, _, _, err := startTestWebhook(t, testGRPCSentinelToken, "true", addr)
	require.NoError(t, err)

	_, err = callSendWebhook(t, addr, "", noopEvent, nil)
	assertUnauthenticated(t, err)

	_, err = callSendWebhook(t, addr, "wrong-"+testGRPCSentinelToken, noopEvent, nil)
	assertUnauthenticated(t, err)

	resp, err := callSendWebhook(t, addr, testGRPCSentinelToken, noopEvent, nil)
	require.NoError(t, err)
	assert.Equal(t, wkhook.EventStatus_Success, resp.Status)
}

func TestWebhookGRPC_NonLoopbackWithoutToken_LogsWarn(t *testing.T) {
	_, _, rec, err := startTestWebhook(t, "", "", "0.0.0.0:0")
	require.NoError(t, err)
	assert.Equal(t, 1, rec.levels()["warn"])
}

func TestWebhookGRPC_StartupLogsNeverContainToken(t *testing.T) {
	cases := []struct {
		name     string
		required string
		wantErr  bool
	}{
		{name: "默认模式", required: ""},
		{name: "强制模式", required: "true"},
		{name: "非法强制值", required: "maybe", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, rec, err := startTestWebhook(t, testGRPCSentinelToken, tc.required, freeLoopbackAddr(t))
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			entries := rec.snapshot()
			require.NotEmpty(t, entries)
			for _, e := range entries {
				assert.NotContains(t, e.text, testGRPCSentinelToken)
			}
		})
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(data)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

func syntheticUIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("u%07d", i)
	}
	return out
}

func offlineEventData(t *testing.T, notify msgOfflineNotify) []byte {
	t.Helper()
	data, err := json.Marshal(notify)
	require.NoError(t, err)
	return data
}

func TestDecodeCompressedRecipients(t *testing.T) {
	list, err := json.Marshal([]string{"u1", "u2"})
	require.NoError(t, err)
	got, err := decodeCompressedRecipients(gzipBytes(t, list))
	require.NoError(t, err)
	assert.Equal(t, []string{"u1", "u2"}, got)

	oversized := []byte(`["` + strings.Repeat("a", maxOfflineRecipientsDecompressedBytes) + `"]`)
	_, err = decodeCompressedRecipients(gzipBytes(t, oversized))
	require.Error(t, err)
}

func TestNormalizeOfflineRecipients(t *testing.T) {
	got, err := normalizeOfflineRecipients([]string{"a", "b", "a", "c", "b"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, got, "de-duplicated, first-occurrence order")

	// 上限按去重后的数量计算。
	atCap := append(syntheticUIDs(maxOfflineRecipients), "u0000000", "u0000001")
	got, err = normalizeOfflineRecipients(atCap)
	require.NoError(t, err)
	assert.Len(t, got, maxOfflineRecipients)

	_, err = normalizeOfflineRecipients(syntheticUIDs(maxOfflineRecipients + 1))
	require.Error(t, err)
}

// 超限事件在进入 pushTo 之前报错：此处 Webhook 没有注入任何服务，
// 若流程继续到 pushTo 会直接失败，而不是返回上限错误。
func TestHandleMsgOffline_RejectsOverLimitRecipients(t *testing.T) {
	w := &Webhook{Log: &recordingLog{}}

	t.Run("压缩列表解压超限", func(t *testing.T) {
		oversized := []byte(`["` + strings.Repeat("a", maxOfflineRecipientsDecompressedBytes) + `"]`)
		data := offlineEventData(t, msgOfflineNotify{Compress: "gzip", CompresssToUIDs: gzipBytes(t, oversized)})
		err := w.handleMsgOffline(data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "上限")
	})

	t.Run("收件人数超限", func(t *testing.T) {
		data := offlineEventData(t, msgOfflineNotify{ToUIDS: syntheticUIDs(maxOfflineRecipients + 1)})
		err := w.handleMsgOffline(data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "上限")
	})

	t.Run("压缩收件人数超限", func(t *testing.T) {
		list, err := json.Marshal(syntheticUIDs(maxOfflineRecipients + 1))
		require.NoError(t, err)
		data := offlineEventData(t, msgOfflineNotify{Compress: "gzip", CompresssToUIDs: gzipBytes(t, list)})
		err = w.handleMsgOffline(data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "上限")
	})
}

func TestHandleOnlineStatus_Bounds(t *testing.T) {
	cfg := config.New()
	ctx := config.NewContext(cfg)
	w := &Webhook{Log: &recordingLog{}, ctx: ctx}

	var (
		mu       sync.Mutex
		received [][]config.OnlineStatus
	)
	ctx.AddOnlineStatusListener(func(s []config.OnlineStatus) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, s)
	})

	t.Run("超限整条拒绝", func(t *testing.T) {
		entries := make([]string, maxOnlineStatusEntries+1)
		for i := range entries {
			entries[i] = fmt.Sprintf("u%07d-1-1", i)
		}
		data, err := json.Marshal(entries)
		require.NoError(t, err)
		require.Error(t, w.handleOnlineStatus(data))
		mu.Lock()
		defer mu.Unlock()
		assert.Empty(t, received, "listeners must not run for a rejected event")
	})

	t.Run("上限内解析结果不变", func(t *testing.T) {
		data, err := json.Marshal([]string{"u1-1-1", "u2-0-0-42-3-4", "malformed"})
		require.NoError(t, err)
		require.NoError(t, w.handleOnlineStatus(data))
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, received, 1)
		assert.Equal(t, []config.OnlineStatus{
			{UID: "u1", DeviceFlag: 1, Online: true},
			{UID: "u2", DeviceFlag: 0, Online: false, SocketID: 42, OnlineCount: 3, TotalOnlineCount: 4},
		}, received[0])
	})
}
