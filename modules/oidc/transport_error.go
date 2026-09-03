package oidc

import (
	"errors"
	"fmt"
	"net/url"
)

// sanitizeTransportErr 去掉传输层错误里携带的完整 URL,再交给上层记录。
//
// 为什么必须有这一层:该 provider 的凭据只能放在 query string 上(IdP 的官方
// 参考实现如此,且未提供替代形态),于是 client_secret / access_token 必然出现
// 在 req.URL 中。而 Go 在传输层失败时返回 *url.Error,其 Error() 会打印
// e.URL —— net/http 构造它时只调用 stripPassword(),那只脱 userinfo 里的密码,
// **query string 原样保留**。这个 error 沿调用链 wrap 上去后会被现有的
// zap.Error(err) 打进日志,凭据随之落盘。
//
// 触发时机恰好最糟:仅在 DNS / 连接 / TLS / 重置这类传输层故障时发生,也就是
// IdP 抖动、日志量最大、日志最可能被外部系统收走的时候。HTTP 层的 4xx/5xx
// 反而不泄漏(oauth2.RetrieveError 只打 status 与 body)。
//
// 设计约束:
//   - 丢弃 e.URL,保留 e.Op(Post/Get 不敏感,且能定位是哪一步)。
//   - 用 %w 保留底层 cause,使 errors.Is/As 仍可用 —— 上层需要区分超时、
//     context 取消与协议错误,剥了 URL 不能把可诊断性一起剥掉。
//   - 非 *url.Error 只补一层 op 上下文,不改变语义。
//
// 放在 provider 边界内做,而不是要求每个调用方"记得别打 err":现网已有 5 处以上
// zap.Error(err),靠 code review 保证不可维护;把不安全的值在源头消灭掉才是可靠的。
func sanitizeTransportErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		// 只保留 op 与底层 cause;ue.URL 整体丢弃。
		return fmt.Errorf("oidc: %s: %s: %w", op, ue.Op, ue.Err)
	}
	return fmt.Errorf("oidc: %s: %w", op, err)
}
