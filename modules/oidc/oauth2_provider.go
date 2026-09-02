package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/oidcboot"
)

// 本文件实现 plain-OAuth2 provider —— 即只跑标准 OAuth2 authorization_code、
// 不提供 OIDC Discovery / id_token / JWKS 的 IdP。
//
// 与标准 OIDC provider 的根本差异在信任锚:那边的 claims 完整性由 IdP 对
// id_token 的签名保证,验签通过即可认定 claims 未被篡改且 iss 可信;这边没有
// 任何签名,claims 的唯一保护是 TLS 传输通道。因此标准路线"免费"获得的校验
// 必须在本文件里逐条手写,见 parseUserInfoEnvelope 的注释。

// userInfoEnvelope 该类 IdP 的 /userinfo 私有响应信封。
//
// 与 OIDC 规范的 /userinfo 不同,claims 不在顶层而是嵌在 data 里,且外层带
// success/code 业务状态 —— 失败同样以 HTTP 200 返回,所以 HTTP 状态码不能
// 作为成功判据。
type userInfoEnvelope struct {
	Success bool         `json:"success"`
	Code    envelopeCode `json:"code"`
	// RequestID 是 IdP 侧的请求追踪号,排查登录失败时是与对方对账的唯一凭据,
	// 因此允许进日志与错误信息(它不是用户数据)。
	RequestID string        `json:"requestId"`
	Data      *userInfoData `json:"data"`
}

// userInfoData 信封内的用户字段。
//
// 刻意**不**声明 email_verified / phone_number_verified:本协议没有 verified
// 语义,响应里即便出现同名字段也不具备可信含义。不声明 = json 解析阶段直接
// 丢弃 = IDTokenClaims 上这两个 bool 恒为零值 false,autolink 因此天然
// fail-closed(见 service.go 的 emailLinkable/phoneLinkable 判定)。
// 采信一个未经验证的邮箱会让 autolink 把新登录者认领到已有账号上。
//
// 同样刻意不声明 iss:issuer 由我方注入,不接受响应体指定(见 parseUserInfoEnvelope)。
type userInfoData struct {
	Sub         string `json:"sub"`
	Nickname    string `json:"nickname"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phone_number"`
}

// envelopeCode 兜底 code 的 string / number 两种序列化形态。
//
// 对方文档把它标为 String、示例写作 "200",但同一份文档里 user_id 就存在
// "表标 Integer、示例是字符串" 的自相矛盾,说明标注不可全信。而 wire type
// 一旦变化就是全站登录失败(本模块 aud 字段历史上踩过同类坑,见
// IDTokenClaims 的注释),所以这里两种都接。
type envelopeCode string

func (c *envelopeCode) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	switch {
	case s == "" || s == "null":
		*c = ""
		return nil
	case strings.HasPrefix(s, `"`):
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			// 不 wrap 原始 err:它可能回显响应片段,而本 error 会被上层 zap.Error 记录。
			return fmt.Errorf("oidc: userinfo envelope: code is not a decodable string")
		}
		*c = envelopeCode(strings.TrimSpace(str))
		return nil
	default:
		var n json.Number
		if err := json.Unmarshal(data, &n); err != nil {
			return fmt.Errorf("oidc: userinfo envelope: code is neither string nor number")
		}
		*c = envelopeCode(n.String())
		return nil
	}
}

// envelopeCodeSuccess 信封内表示业务成功的 code 值。
const envelopeCodeSuccess = "200"

// envelopeError 信封被拒时的错误。
//
// 只携带 code 与 requestId —— 两者都是 IdP 侧的协议/追踪字段,不含用户数据,
// 可以安全进日志。**绝不**携带 message 或 data:message 实测会包含手机号一类
// PII,data 含 subject 等身份标识,而本 error 一路 wrap 上去会被现有的
// zap.Error(err) 打进日志。
type envelopeError struct {
	reason    string
	Code      string
	RequestID string
}

func (e *envelopeError) Error() string {
	return fmt.Sprintf("oidc: userinfo envelope rejected: %s (code=%q request_id=%q)",
		e.reason, e.Code, e.RequestID)
}

// parseUserInfoEnvelope 把 plain-OAuth2 IdP 的 /userinfo 响应体解析为归一化 claims。
//
// 这是该 provider 唯一的身份来源,因此它同时是解析器和信任边界。标准 OIDC 靠
// 验签免费拿到的三件事,这里必须自己做,漏一条就是可登录的安全缺陷:
//
//  1. success + code 判定 —— 失败以 HTTP 200 承载,不判等于把失败当登录成功。
//  2. subject 非空 —— user_oidc_identity.subject 是 NOT NULL DEFAULT ” 且带
//     UNIQUE(issuer,subject);放进空串会让所有空 sub 用户塌成同一行,
//     互相登进对方账号(账号接管,不只是脏数据)。
//  3. issuer 由入参注入 —— 本协议无 iss claim。若从响应体取值,IdP 侧一次
//     配置变更(或中间人)就能把身份写进别的命名空间,绕过 (issuer,subject)
//     的唯一性语义。
//
// issuer 必须是调用方从配置读出的稳定常量;它一旦上线不可更改,否则全部存量
// 绑定会在登录第一步 miss,等于全员按新账号重建。
func parseUserInfoEnvelope(body []byte, issuer string) (*IDTokenClaims, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, &envelopeError{reason: "empty response body"}
	}

	var env userInfoEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		// 同样不回显 err:json 的报错可能带上响应片段。
		return nil, &envelopeError{reason: "response is not a decodable envelope"}
	}

	// (1) 业务状态。success 缺失时 Go 零值 false,即 fail-closed。
	if !env.Success || string(env.Code) != envelopeCodeSuccess {
		return nil, &envelopeError{
			reason:    "IdP reported a non-success status",
			Code:      string(env.Code),
			RequestID: env.RequestID,
		}
	}

	if env.Data == nil {
		return nil, &envelopeError{
			reason:    "envelope carries no data object",
			Code:      string(env.Code),
			RequestID: env.RequestID,
		}
	}

	// (2) subject 非空。
	subject := strings.TrimSpace(env.Data.Sub)
	if subject == "" {
		return nil, &envelopeError{
			reason:    "subject is empty",
			Code:      string(env.Code),
			RequestID: env.RequestID,
		}
	}

	// (2b) subject 形态守卫 —— 见 errSubjectLooksLikeEmployeeNo 的说明。
	if err := checkSubjectShape(subject); err != nil {
		return nil, &envelopeError{
			reason:    err.Error(),
			Code:      string(env.Code),
			RequestID: env.RequestID,
		}
	}

	// (3) issuer 注入,不从响应体取。
	return &IDTokenClaims{
		Issuer:      issuer,
		Subject:     subject,
		Name:        strings.TrimSpace(env.Data.Nickname),
		Email:       strings.TrimSpace(env.Data.Email),
		PhoneNumber: strings.TrimSpace(env.Data.PhoneNumber),
		// EmailVerified / PhoneVerified 有意留零值 false —— 见 userInfoData 注释。
	}, nil
}

// upstreamLogoutPathPrefix 该 IdP 单点登出端点的固定路径前缀,appId 作为
// 最后一个路径段拼在其后。
const upstreamLogoutPathPrefix = "public/sp/slo"

// upstreamAppIDRe 复用 pkg/oidcboot 的唯一定义。
//
// 曾经这里另有一份更严的正则,于是启动期放过、运行期拒绝 —— 而运行期这边
// LogoutURL 吞掉错误返回 ("", false),登出静默降级成"只清本地"。判据只能有一份。
var upstreamAppIDRe = oidcboot.AppIDPattern

// buildUpstreamLogoutURL 拼该 IdP 的单点登出地址:
//
//	{base}/public/sp/slo/{appId}?redirect_url={redirect}
//
// 与 OIDC RP-Initiated Logout 的差异(逐条都要求不同的处理):
//   - appId 在**路径段**,不是 query 参数;
//   - 回跳参数名是 redirect_url,不是 post_logout_redirect_uri;
//   - 没有 id_token_hint 可送 —— IdP 只能靠浏览器带上的自身域 cookie 判断
//     要结束谁的会话。因此该 URL 必须由**浏览器顶层导航**访问:服务端代理
//     请求不携带用户的 IdP cookie,等于谁也没登出。
//
// redirect 必须由运维在配置里写死、绝不接受调用方传入:单一取值本身就是白名单,
// 因此无需再做一次 redirect 校验。若改成可由请求指定,就是一个开放重定向,
// 而且它挂在指向 IdP 的 URL 上,钓鱼价值高于普通开放重定向。
//
// 返回的 URL 不含任何凭据,可以安全写日志(与 RP-Initiated Logout 相反)。
func buildUpstreamLogoutURL(baseURL, appID, redirect string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	appID = strings.TrimSpace(appID)
	redirect = strings.TrimSpace(redirect)

	if baseURL == "" {
		return "", fmt.Errorf("oidc: upstream logout: base URL is empty")
	}
	if appID == "" {
		return "", fmt.Errorf("oidc: upstream logout: app id is empty")
	}
	if !upstreamAppIDRe.MatchString(appID) {
		// 不回显 appID 本身以外的内容;它是配置值,回显有助于运维定位。
		return "", fmt.Errorf("oidc: upstream logout: app id %q is not URL-safe (allowed: letters, digits, '-', '_')", appID)
	}
	if redirect == "" {
		// 没有回跳地址,用户登出后会停在 IdP 页面回不来。视为配置错误,
		// 而不是"省略该参数"的可降级项。
		return "", fmt.Errorf("oidc: upstream logout: redirect target is empty")
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("oidc: upstream logout: base URL is unparseable")
	}
	if !u.IsAbs() || u.Host == "" {
		return "", fmt.Errorf("oidc: upstream logout: base URL must be absolute")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		// 拦 javascript: / data: 一类 scheme —— 该 URL 会成为浏览器顶层跳转目标。
		return "", fmt.Errorf("oidc: upstream logout: base URL scheme %q is not http(s)", u.Scheme)
	}

	// 用 path.Join 归一化,避免 base 带尾斜杠时产生 "//"。appID 已过白名单,
	// 此处不会引入 ".." 段。
	u.Path = "/" + strings.TrimLeft(path.Join(u.Path, upstreamLogoutPathPrefix, appID), "/")

	// 回跳地址整体 percent-encode:它自身可能带 query,其 & 不能被 IdP
	// 当作自己的参数分隔符。Encode() 会正确处理。
	q := url.Values{}
	q.Set("redirect_url", redirect)
	u.RawQuery = q.Encode()
	u.Fragment = ""

	return u.String(), nil
}

// ---- provider 实现 ----

// upstreamAuthorizePath / upstreamTokenPath authorize 与 token 端点的固定路径。
const (
	upstreamAuthorizePath = "oauth/authorize"
	upstreamTokenPath     = "oauth/token"
)

// oauth2ProviderConfig 构造 plain-OAuth2 provider 所需的配置。
type oauth2ProviderConfig struct {
	// Issuer 写入 user_oidc_identity.issuer 的身份命名空间。本协议没有 iss
	// claim,该值完全由运维决定,且上线后不可更改(改了等于全员重建账号)。
	// 测试与生产必须取不同值,否则测试数据会污染生产的身份判断。
	Issuer string
	// BaseURL IdP 的站点根,authorize / token / userinfo / 登出路径都拼在其后。
	BaseURL string

	ClientID     string
	ClientSecret string
	RedirectURI  string
	Scopes       []string

	// AppID 单点登出用的应用标识。它**不是** ClientID —— IdP 侧是两个不同的
	// 注册值,由同一批管理员分别下发,且可能每环境不同。缺失时 LogoutURL
	// 返回 ("", false),登出降级为仅清本地。
	AppID string
	// PostLogoutRedirectURI 登出后回跳地址,必须由运维写死。单一取值即白名单,
	// 因此无需再做 redirect 校验;若改为接受调用方传入则是开放重定向。
	PostLogoutRedirectURI string
}

// oauth2Provider 只讲标准 OAuth2 authorization_code 的 IdP 适配器。
type oauth2Provider struct {
	cfg oauth2ProviderConfig
}

// newOAuth2Provider 校验配置并构造 provider。
//
// 启动期 fail-loud:配置不全就拒绝构造,而不是运行期才在某条登录路径上报错。
// 半配置状态的 provider 会让一部分流程可用、另一部分静默失败,极难排查。
func newOAuth2Provider(cfg oauth2ProviderConfig) (*oauth2Provider, error) {
	cfg.Issuer = strings.TrimSpace(cfg.Issuer)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	cfg.ClientSecret = strings.TrimSpace(cfg.ClientSecret)
	cfg.RedirectURI = strings.TrimSpace(cfg.RedirectURI)
	cfg.AppID = strings.TrimSpace(cfg.AppID)
	cfg.PostLogoutRedirectURI = strings.TrimSpace(cfg.PostLogoutRedirectURI)

	for _, f := range []struct {
		name, val string
	}{
		{"issuer", cfg.Issuer},
		{"base URL", cfg.BaseURL},
		{"client id", cfg.ClientID},
		{"client secret", cfg.ClientSecret},
		{"redirect URI", cfg.RedirectURI},
	} {
		if f.val == "" {
			return nil, fmt.Errorf("oidc: oauth2 provider: %s is required", f.name)
		}
	}
	if err := validateAbsoluteHTTPURL("base URL", cfg.BaseURL); err != nil {
		return nil, err
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"read"}
	}
	return &oauth2Provider{cfg: cfg}, nil
}

// validateAbsoluteHTTPURL 要求绝对 http(s) 地址。
// 拦相对地址与 javascript:/data: 一类 scheme —— 这些值会被拼进浏览器跳转目标。
func validateAbsoluteHTTPURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("oidc: oauth2 provider: %s is unparseable", field)
	}
	if !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("oidc: oauth2 provider: %s must be an absolute URL", field)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("oidc: oauth2 provider: %s scheme %q is not http(s)", field, u.Scheme)
	}
	return nil
}

func (p *oauth2Provider) Kind() ProviderKind { return KindOAuth2 }

func (p *oauth2Provider) Issuer() string { return p.cfg.Issuer }

// Capabilities 诚实声明本协议缺失的能力,供上层跳过对应步骤。
func (p *oauth2Provider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		PKCE:  false, // authorize 端点无 code_challenge 参数
		Nonce: false, // 没有可把 nonce 带回来的签名载荷
		// token 响应不含 id_token → 上层不得缓存 id_token、不得走
		// RP-Initiated Logout(见 LogoutURL 用的是厂商私有登出端点)。
		IDToken: false,
		// 会返回 refresh_token,但对方文档从未给出刷新端点。声明 false 是为了
		// 让 sync worker 整段被跳过 —— 否则它会每 15 分钟空转一次。
		RefreshToken: false,
		// 只有 /userinfo 一个身份来源,没有第二份可比对。
		CrossCheckSub: false,
		// 有前端跳转式单点登出端点(需另配 AppID)。
		UpstreamLogout: true,
	}
}

// AuthCodeURL 构造 authorize 跳转地址。
//
// 有意**不**发送 nonce / code_challenge:协议里没有这两个参数,对方对未注册
// 参数的处理未经验证,而它们在这里也提供不了任何保护。调用方按统一的
// AuthCodeParams 传入,由本实现决定丢弃。
func (p *oauth2Provider) AuthCodeURL(params AuthCodeParams) (string, error) {
	state := strings.TrimSpace(params.State)
	if state == "" {
		// state 是本协议下唯一的 CSRF 绑定 —— IdP 文档把它标为可选,其参考
		// 实现甚至完全不读它,防护责任全在我方。空 state 属于编程错误,
		// 不能静默产出一个无保护的 authorize URL。
		return "", fmt.Errorf("oidc: oauth2 provider: state is required")
	}
	u, err := url.Parse(p.cfg.BaseURL)
	if err != nil {
		return "", fmt.Errorf("oidc: oauth2 provider: base URL is unparseable")
	}
	u.Path = "/" + strings.TrimLeft(path.Join(u.Path, upstreamAuthorizePath), "/")

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(p.cfg.Scopes, " "))
	q.Set("client_id", p.cfg.ClientID)
	q.Set("redirect_uri", p.cfg.RedirectURI)
	q.Set("state", state)
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String(), nil
}

// LogoutURL 返回厂商私有的单点登出地址。
//
// 与 RP-Initiated Logout 不同,这里不需要(也无法提供)id_token_hint,所以
// hint.RawIDToken 被忽略。AppID 或回跳地址缺失时返回 ("", false),让上层
// 降级为"仅清本地",而不是产出一个畸形 URL。
func (p *oauth2Provider) LogoutURL(_ context.Context, _ LogoutHint) (string, bool) {
	if p.cfg.AppID == "" || p.cfg.PostLogoutRedirectURI == "" {
		return "", false
	}
	raw, err := buildUpstreamLogoutURL(p.cfg.BaseURL, p.cfg.AppID, p.cfg.PostLogoutRedirectURI)
	if err != nil {
		return "", false
	}
	return raw, true
}

// upstreamUserInfoPath 该 IdP 的 userinfo 端点路径(厂商私有,非 OIDC 规范路径)。
const upstreamUserInfoPath = "api/bff/v1.2/oauth2/userinfo"

const (
	// oauth2HTTPTimeout 单次上游调用的整体超时。
	// 不复用 pkg/network 的 Get/Post —— 它们走 sendgrid rest 的默认 client,
	// 那个 client **没有超时**,被吊死的上游会永久占住 goroutine。
	oauth2HTTPTimeout = 10 * time.Second
	// oauth2MaxResponseBytes 上游响应体上限。userinfo/token 响应都是几百字节,
	// 设上限是为了不让异常的上游(或中间设备的错误页)把内存吃光。
	oauth2MaxResponseBytes = 1 << 20 // 1 MiB
)

// httpClient 返回本 provider 专用的 client。
//
// 禁用自动跟随重定向:token /userinfo 端点都没有合法的 3xx 路径,允许跟随会
// 让被攻陷或错配的上游把我方服务端请求打到任意地址(SSRF 内网探测/云元数据),
// 且凭据在 query string 上会被自动带到重定向目标(Go net/http 仅对跨域 https→http
// 降级剥离 Authorization 头,但我们的凭据在 URL 上而不在 header,任何重定向都会
// 重放——这是比 SSRF 更糟的凭据外泄路径)。非 2xx 统一由上层当 transport error 处理。
func (p *oauth2Provider) httpClient() *http.Client {
	return &http.Client{
		Timeout: oauth2HTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// oauth2TokenResponse token 端点的响应。
//
// 有意不解析 expires_in:该字段的 wire type 在对方文档里自相矛盾(表标 String、
// 示例是 number),而我们**根本不需要它** —— access_token 只在 callback 内活几百
// 毫秒,拿完 userinfo 即丢弃,不落库、不参与我方会话生命周期(我方会话由
// pkg/auth 独立签发)。少解析一个字段就少一处 wire-type 变更导致全站登录失败的风险。
type oauth2TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// Exchange 用授权码换取 access_token。
//
// 形态严格照对方的官方参考实现:POST + 全部参数在 **query string** + 空 body。
// 那是唯一被验证过的调用方式;form body 在 Spring 侧理论可行,但未经验证,
// 上游网关可能只读 query 或只对 query 签名,所以不赌。
//
// 即使 body 为空也带 Content-Type: application/x-www-form-urlencoded ——
// 参考实现如此,且对方错误码表里列了 415 UnsupportedMediaType,说明它确实会
// 因 Content-Type 拒请求。
//
// codeVerifier 被忽略:本协议没有 PKCE。
func (p *oauth2Provider) Exchange(ctx context.Context, code, _ string) (*TokenSet, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, fmt.Errorf("oidc: oauth2 provider: authorization code is required")
	}

	u, err := url.Parse(p.cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: oauth2 provider: base URL is unparseable")
	}
	u.Path = "/" + strings.TrimLeft(path.Join(u.Path, upstreamTokenPath), "/")

	// url.Values.Encode 会正确 percent-encode。参考实现用 String.format 裸拼,
	// 那会在 client_secret / redirect_uri 含特殊字符时产生错误请求 —— 不照抄。
	q := url.Values{}
	q.Set("grant_type", "authorization_code")
	q.Set("code", code)
	q.Set("client_id", p.cfg.ClientID)
	q.Set("client_secret", p.cfg.ClientSecret)
	q.Set("redirect_uri", p.cfg.RedirectURI)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), http.NoBody)
	if err != nil {
		// 不回显 err:它会带上含 client_secret 的完整 URL。
		return nil, fmt.Errorf("oidc: oauth2 provider: cannot build token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	body, err := p.doJSON(req, "exchange")
	if err != nil {
		return nil, err
	}

	var tr oauth2TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("oidc: oauth2 provider: token response is not decodable JSON")
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		return nil, fmt.Errorf("oidc: oauth2 provider: token response carries no access_token")
	}
	return &TokenSet{
		AccessToken:  tr.AccessToken,
		TokenType:    tr.TokenType,
		RefreshToken: tr.RefreshToken,
		// RawIDToken 留空:本协议无 id_token。
		// Expiry 留零值:见 oauth2TokenResponse 注释,我方不消费上游有效期。
	}, nil
}

// Identity 用 access_token 拉 /userinfo 并解析成归一化 claims。
//
// token 走 query 而非 Authorization 头 —— 这是该 IdP 的形态,也是 go-oidc 的
// UserInfo 无法复用的原因之一(它只会发 Bearer 头,且只解顶层 sub)。
//
// 本方法是该 provider 唯一的身份来源,所以协议层的全部校验都在这里完成:
// HTTP 状态、私有信封的 success/code、subject 非空。对上层只暴露"可信"或 error。
func (p *oauth2Provider) Identity(ctx context.Context, tok *TokenSet) (*IdentityClaims, error) {
	if tok == nil || strings.TrimSpace(tok.AccessToken) == "" {
		return nil, fmt.Errorf("oidc: oauth2 provider: access token is required")
	}

	u, err := url.Parse(p.cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: oauth2 provider: base URL is unparseable")
	}
	u.Path = "/" + strings.TrimLeft(path.Join(u.Path, upstreamUserInfoPath), "/")
	q := url.Values{}
	q.Set("access_token", tok.AccessToken)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("oidc: oauth2 provider: cannot build userinfo request")
	}
	req.Header.Set("Accept", "application/json")

	body, err := p.doJSON(req, "userinfo")
	if err != nil {
		return nil, err
	}
	// 信封解析同时承担信任边界职责,issuer 在此注入。
	return parseUserInfoEnvelope(body, p.cfg.Issuer)
}

// doJSON 发请求并读回响应体,统一处理超时、响应体上限与错误脱敏。
//
// op 只用于错误上下文(exchange / userinfo),不含任何用户数据。
func (p *oauth2Provider) doJSON(req *http.Request, op string) ([]byte, error) {
	resp, err := p.httpClient().Do(req)
	if err != nil {
		// 传输层失败:*url.Error 会带完整 URL(含 query 里的凭据),必须脱敏。
		return nil, sanitizeTransportErr(op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 限制读取量:异常上游或中间设备的错误页可能非常大。
	body, err := io.ReadAll(io.LimitReader(resp.Body, oauth2MaxResponseBytes+1))
	if err != nil {
		return nil, sanitizeTransportErr(op, err)
	}
	if len(body) > oauth2MaxResponseBytes {
		return nil, fmt.Errorf("oidc: oauth2 provider: %s response exceeds %d bytes", op, oauth2MaxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		// 白名单式提取:只带上游的 error 枚举码,丢弃 error_description。
		//
		// 上游在协议错误时返回 Spring Security 的标准形态
		// {"error":"...","error_description":"..."},而不是文档描述的成功信封。
		// error 是枚举值(invalid_grant / invalid_client / unauthorized / ...),
		// 是区分"code 无效"与"凭据被拒"的唯一依据,联调和线上排查都靠它,必须带。
		// error_description 则不能带 —— 实测见过其中含手机号一类用户数据,
		// 而本 error 会一路 wrap 到 zap.Error 落进日志。
		if code := upstreamErrorCode(body); code != "" {
			return nil, fmt.Errorf("oidc: oauth2 provider: %s returned HTTP %d (upstream error=%q)",
				op, resp.StatusCode, code)
		}
		return nil, fmt.Errorf("oidc: oauth2 provider: %s returned HTTP %d", op, resp.StatusCode)
	}
	return body, nil
}

// upstreamErrorCode 从上游错误响应里取出 OAuth2 的 error 枚举码。
//
// 解析失败(例如中间设备返回 HTML 错误页)时返回空串,由调用方退回只报状态码 ——
// 不能因为错误体形态意外就让整个请求以 panic 或误导性信息收场。
//
// 刻意只声明 Error 一个字段:多声明一个 ErrorDescription 就会给后来人
// "顺手打进日志"的机会,而那里可能有用户数据。
func upstreamErrorCode(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	code := strings.TrimSpace(e.Error)
	// 上游 error 码是协议枚举,长度有限;异常长的值说明这不是我们预期的结构,
	// 宁可丢弃也不要把一段不明文本带进日志。
	if len(code) > 64 {
		return ""
	}
	return code
}

// IdentityFromClientCredential 把客户端出示的 access_token 拿去问 /userinfo。
//
// 这个协议里 access_token 是不透明串,没有任何可本地验证的结构 —— 唯一能确立
// 身份的办法就是拿它调上游。所以这条路径必然外呼,可用性跟着 IdP 走;
// 这是协议事实,不是实现选择。
//
// 刻意**不**尝试把它当 JWT 解析:不透明串完全可能恰好是 JWT 形态(上游用什么
// 编码是它的内部实现),按形态猜会把一个未经我方验证的载荷当成身份来源。
func (p *oauth2Provider) IdentityFromClientCredential(ctx context.Context, raw string) (*IdentityClaims, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("oidc: oauth2 provider: credential is empty")
	}
	return p.Identity(ctx, &TokenSet{AccessToken: raw})
}
