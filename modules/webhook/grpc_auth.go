package webhook

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	// grpcAuthTokenEnv 是 webhook gRPC 监听器的共享 token。
	grpcAuthTokenEnv = "TS_GRPC_AUTH_TOKEN"
	// grpcAuthRequiredEnv 显式要求 gRPC 认证。为 true 时未配置 token 则拒绝启动；
	// 默认 false，保持「配了 token 才校验」的既有行为（WuKongIM 2.2.x 不发送 token）。
	grpcAuthRequiredEnv = "TS_GRPC_AUTH_REQUIRED"
	// grpcAuthMetadataKey 是调用方携带 token 的 metadata key。
	grpcAuthMetadataKey = "auth_token"
)

// grpcAuthConfig 是 webhook gRPC 监听器的认证配置。
type grpcAuthConfig struct {
	token    string
	required bool
}

// loadGRPCAuthConfig 从环境变量解析认证配置。
// TS_GRPC_AUTH_REQUIRED 取值非法，或要求认证但未配置 token 时返回错误（fail closed）。
// 返回的错误只包含环境变量名，不包含 token。
func loadGRPCAuthConfig(getenv func(string) string) (grpcAuthConfig, error) {
	cfg := grpcAuthConfig{token: getenv(grpcAuthTokenEnv)}
	if raw := strings.TrimSpace(getenv(grpcAuthRequiredEnv)); raw != "" {
		required, err := strconv.ParseBool(raw)
		if err != nil {
			return grpcAuthConfig{}, fmt.Errorf("%s has an invalid boolean value %q", grpcAuthRequiredEnv, raw)
		}
		cfg.required = required
	}
	if cfg.required && cfg.token == "" {
		return grpcAuthConfig{}, fmt.Errorf("%s=true requires %s to be set", grpcAuthRequiredEnv, grpcAuthTokenEnv)
	}
	return cfg, nil
}

// grpcAuthInterceptor 返回一个 gRPC 一元拦截器，验证请求 metadata 中的 auth_token。
// expectedToken 为空时拒绝所有请求。比较前两边都先做 SHA-256，得到等长摘要后再做
// 常量时间比较，响应耗时既不随内容也不随长度变化。
// 所有失败统一返回同一个 Unauthenticated 错误，不区分具体原因。
func grpcAuthInterceptor(expectedToken string) grpc.UnaryServerInterceptor {
	configured := expectedToken != ""
	expectedSum := sha256.Sum256([]byte(expectedToken))
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !configured || !grpcAuthTokenMatches(ctx, expectedSum) {
			return nil, status.Error(codes.Unauthenticated, "invalid or missing auth_token")
		}
		return handler(ctx, req)
	}
}

func grpcAuthTokenMatches(ctx context.Context, expectedSum [sha256.Size]byte) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	tokens := md.Get(grpcAuthMetadataKey)
	if len(tokens) == 0 {
		return false
	}
	gotSum := sha256.Sum256([]byte(tokens[0]))
	return subtle.ConstantTimeCompare(gotSum[:], expectedSum[:]) == 1
}

// isLoopbackListenAddr 判断监听地址是否只绑定在本机回环地址上。
// 空 host（如 ":6979"）、0.0.0.0、:: 表示所有网卡，返回 false。
func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
