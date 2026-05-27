package i18n

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"golang.org/x/text/language"
)

const (
	// HeaderOctoLang 是受信网关/服务间调用使用的语言头。
	HeaderOctoLang = "X-Octo-Lang"
	// CookieLanguage 是前端与公开静态页共享的语言 cookie 名。
	CookieLanguage = "i18n_lang"
	// QueryLanguage 是 URL 显式语言选择参数。
	QueryLanguage = "lang"
)

// LanguageSource 描述命中 D6 语言协商链的哪一级。
type LanguageSource string

const (
	LanguageSourceTrustedHeader LanguageSource = "trusted_header"
	LanguageSourceQuery         LanguageSource = "query"
	LanguageSourceCookie        LanguageSource = "cookie"
	LanguageSourceUser          LanguageSource = "user"
	LanguageSourceAccept        LanguageSource = "accept_language"
	LanguageSourceDefault       LanguageSource = "default"
)

var (
	supportedLanguageTags = []language.Tag{
		language.MustParse(SourceLanguage),
		language.MustParse("zh-CN"),
	}
	supportedLanguageMatcher = language.NewMatcher(supportedLanguageTags)
)

// LanguageNegotiationOptions 控制单次语言协商。
type LanguageNegotiationOptions struct {
	DefaultLanguage        string
	TrustedLangHeaderCIDRs []*net.IPNet
	TrustedProxyCIDRs      []*net.IPNet
	UserLanguage           string
}

// NegotiateLanguage 按 D6 优先级协商请求语言：
// trusted X-Octo-Lang > URL lang > cookie i18n_lang > user.language >
// Accept-Language > default language。
//
// X-Octo-Lang 默认根据 TCP RemoteAddr 命中 TrustedLangHeaderCIDRs 时采纳；
// 配置 TrustedProxyCIDRs 时，先对 X-Forwarded-For + RemoteAddr 从右向左
// 剥离可信反代，再用剥离后的调用方 IP 判定 TrustedLangHeaderCIDRs。
func NegotiateLanguage(r *http.Request, opts LanguageNegotiationOptions) LanguageDecision {
	defaultLang := normalizeDefaultLanguage(opts.DefaultLanguage)
	if r == nil {
		return LanguageDecision{Language: defaultLang, Source: LanguageSourceDefault}
	}

	if IsTrustedLangHeaderRequest(r, opts.TrustedLangHeaderCIDRs, opts.TrustedProxyCIDRs) {
		if lang, ok := MatchSupportedLanguage(r.Header.Get(HeaderOctoLang)); ok {
			return LanguageDecision{Language: lang, Source: LanguageSourceTrustedHeader}
		}
	}
	if lang, ok := MatchSupportedLanguage(r.URL.Query().Get(QueryLanguage)); ok {
		return LanguageDecision{Language: lang, Source: LanguageSourceQuery}
	}
	if cookie, err := r.Cookie(CookieLanguage); err == nil {
		if lang, ok := MatchSupportedLanguage(cookie.Value); ok {
			return LanguageDecision{Language: lang, Source: LanguageSourceCookie}
		}
	}
	if lang, ok := MatchSupportedLanguage(opts.UserLanguage); ok {
		return LanguageDecision{Language: lang, Source: LanguageSourceUser}
	}
	if lang, ok := matchAcceptLanguage(r.Header.Get("Accept-Language")); ok {
		return LanguageDecision{Language: lang, Source: LanguageSourceAccept}
	}
	return LanguageDecision{Language: defaultLang, Source: LanguageSourceDefault}
}

// MatchSupportedLanguage 将输入语言规整到首期支持矩阵（en-US / zh-CN）。
func MatchSupportedLanguage(raw string) (string, bool) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "_", "-"))
	if raw == "" {
		return "", false
	}
	tag, err := language.Parse(raw)
	if err != nil {
		return "", false
	}
	_, idx, confidence := supportedLanguageMatcher.Match(tag)
	if confidence == language.No {
		return "", false
	}
	return supportedLanguageTags[idx].String(), true
}

// ParseCIDRList 解析逗号分隔 CIDR 列表，例如 "10.0.0.0/8,172.16.0.0/12"。
func ParseCIDRList(value string) ([]*net.IPNet, error) {
	parts := strings.Split(value, ",")
	out := make([]*net.IPNet, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("parse CIDR %q: %w", part, err)
		}
		out = append(out, network)
	}
	return out, nil
}

// IsTrustedLangHeaderRequest 判断请求调用方 IP 是否命中受信语言头 CIDR。
func IsTrustedLangHeaderRequest(r *http.Request, cidrs []*net.IPNet, trustedProxyCIDRs ...[]*net.IPNet) bool {
	if r == nil || len(cidrs) == 0 {
		return false
	}
	var proxyCIDRs []*net.IPNet
	if len(trustedProxyCIDRs) > 0 {
		proxyCIDRs = trustedProxyCIDRs[0]
	}
	ip, ok := trustedCallerIP(r, proxyCIDRs)
	if !ok {
		return false
	}
	for _, cidr := range cidrs {
		if cidr != nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func trustedCallerIP(r *http.Request, trustedProxyCIDRs []*net.IPNet) (net.IP, bool) {
	remoteIP, ok := remoteAddrIP(r.RemoteAddr)
	if !ok {
		return nil, false
	}
	if len(trustedProxyCIDRs) == 0 || !cidrListContains(trustedProxyCIDRs, remoteIP) {
		return remoteIP, true
	}

	chain := forwardedForIPs(r.Header.Get("X-Forwarded-For"))
	chain = append(chain, remoteIP)
	for len(chain) > 1 && cidrListContains(trustedProxyCIDRs, chain[len(chain)-1]) {
		chain = chain[:len(chain)-1]
	}
	if len(chain) == 0 {
		return remoteIP, true
	}
	return chain[len(chain)-1], true
}

func forwardedForIPs(raw string) []net.IP {
	parts := strings.Split(raw, ",")
	out := make([]net.IP, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ip := net.ParseIP(part)
		if ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

func cidrListContains(cidrs []*net.IPNet, ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, cidr := range cidrs {
		if cidr != nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func normalizeDefaultLanguage(raw string) string {
	if lang, err := ResolveDefaultLanguage(raw); err == nil {
		return lang
	}
	return DefaultLanguage
}

func matchAcceptLanguage(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "", false
	}
	tags, _, err := language.ParseAcceptLanguage(raw)
	if err != nil || len(tags) == 0 {
		return "", false
	}
	_, idx, confidence := supportedLanguageMatcher.Match(tags...)
	if confidence == language.No {
		return "", false
	}
	return supportedLanguageTags[idx].String(), true
}

func remoteAddrIP(remoteAddr string) (net.IP, bool) {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return nil, false
	}
	if ip, _, err := net.SplitHostPort(remoteAddr); err == nil {
		parsed := net.ParseIP(ip)
		return parsed, parsed != nil
	}
	parsed := net.ParseIP(remoteAddr)
	return parsed, parsed != nil
}
