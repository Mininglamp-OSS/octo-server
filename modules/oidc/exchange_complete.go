package oidc

// exchange_complete.go — 两个 token-exchange 端点在"身份已验证之后"共用的尾段。
//
// 为什么必须共用:这段逻辑是 ResolveOrLink → IssueSession → identity 写入
// (含竞态恢复)→ 审计 → 响应,每一步的错误处理都是安全关键。它曾经在
// api_exchange.go 与 api_exchange_jwt.go 里各存一份 ~65 行的逐行拷贝,后果不是
// 代码重复而是**同一个缺陷犯两遍**:竞态恢复的赢家会话在两份拷贝里都被丢弃,
// 手机号脱敏在其中一份里漏掉。callback 路径那第三份则各自是对的 —— 也就是说
// 修一处永远不会自动修另一处。
//
// 两个端点真正的差异只有观测口径(metric 家族、审计事件、日志前缀),
// 用 exchangeFlavour 把它显式参数化。

import (
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// verifiedIdentity 一次**已通过验证**的身份,连同把它兑换成我方会话所需的上下文。
//
// 构造它的两条路径信任来源不同(上游 /userinfo 响应 vs 本地 HS256 验签),
// 但一旦构造出来,后续处理必须完全一致 —— 差异化处理是上一版两个缺陷的来源。
type verifiedIdentity struct {
	// claims 协议中立的身份声明。issuer 一定来自我方配置,不来自任何响应体。
	claims *IdentityClaims

	deviceFlag uint8
	clientIP   string
	traceID    string
	state      *StateData

	// auditDetail 成功审计行的备注。bearer JWT 路径填 domainAccount(人类可读,
	// 便于与上游对账);OAuth2 路径留空(subject 是长数字串,进审计无意义,
	// 且它已经以 hash 形式进了日志)。
	auditDetail string
}

// exchangeFlavour 两个端点的观测口径。除此之外它们共用同一段实现。
type exchangeFlavour struct {
	// logName 进日志的端点名,例如 "exchange" / "exchange-jwt"。
	logName string
	// result 终态计数器,label 名固定为 result。
	result *prometheus.CounterVec
	// eventOK / eventFail 该端点的审计事件对。
	//
	// **必须按调用方区分**:recoverFromIdentityRace 曾硬编码 EventCallbackFail,
	// 于是 exchange 的竞态在审计表里显示成"callback 失败",把事后排查引向一条
	// 请求从未走过的路径 —— 而审计行存在的唯一意义就是这种排查。
	eventOK   AuditEvent
	eventFail AuditEvent
}

// completeExchange 把一个已验证身份兑换成我方会话并写响应。
//
// 失败一律回同一个 401 码(anti-enumeration):客户端不该能从响应里区分
// "这个人不存在" / "建号被拒" / "DB 挂了"。具体原因只进日志与审计。
func (o *OIDC) completeExchange(c *wkhttp.Context, vi verifiedIdentity, fl exchangeFlavour) {
	ctx := c.Request.Context()
	claims := vi.claims

	res, err := o.service.ResolveOrLink(ctx, claims)
	if err != nil {
		fl.result.WithLabelValues("resolve_fail").Inc()
		o.Warn("OIDC "+fl.logName+": resolve failed",
			zap.String("trace_id", vi.traceID), zap.String("ip", vi.clientIP),
			zap.String("sub", subHash(claims.Subject)), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
		return
	}

	// 手机号归一化:拿不准归属地时返回空对,按"没有手机号"处理(见 normalizePhone)。
	zone, phone := normalizePhone(claims.PhoneNumber)
	if claims.PhoneNumber != "" && phone == "" {
		// 只打打码尾号与长度:完整号码是 PII,日志留存期远长于它的用途,
		// 而排查这类问题只需要"是哪一类号码"而不是"是谁的号码"。
		o.Warn("OIDC "+fl.logName+" phone number dropped: cannot determine country code",
			zap.String("trace_id", vi.traceID),
			zap.String("idp_phone_masked", maskPhoneForBind(claims.PhoneNumber)),
			zap.Int("idp_phone_len", len(claims.PhoneNumber)))
	}

	issueReq := IssueSessionReq{
		UID:              res.UID,
		CreateUser:       res.IsNew,
		Name:             claims.Name,
		Email:            claims.Email,
		Phone:            phone,
		Zone:             zone,
		DeviceFlag:       vi.deviceFlag,
		PublicIP:         vi.clientIP,
		TrustedSSOCreate: res.IsNew,
	}
	sessResp, err := o.service.IssueSession(ctx, issueReq)
	if err != nil {
		fl.result.WithLabelValues("issue_fail").Inc()
		o.Error("OIDC "+fl.logName+": issue session failed",
			zap.String("trace_id", vi.traceID), zap.String("ip", vi.clientIP),
			zap.String("sub", subHash(claims.Subject)), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
		return
	}

	// 新用户补写 identity 行。并发首登由 uk_issuer_subject +
	// recoverFromIdentityRace 兜底(与 callback 路径同一个函数)。
	//
	// autoJoinUID:本次请求真正建成、且 identity 行落库成功的账号,用于随后自动
	// 加入运维配置的初始 Space(task oidc-auto-join-initial-space)。竞态输家刻意
	// 不填 —— 理由见下面 race 分支的注释,与 callback 路径同一套语义。
	autoJoinUID := ""
	if res.IsNew && sessResp.UID != "" {
		autoJoinUID = sessResp.UID
		if err := o.store.Insert(&IdentityModel{
			UID:           sessResp.UID,
			Issuer:        claims.Issuer,
			Subject:       claims.Subject,
			Email:         claims.Email,
			EmailVerified: boolToInt(claims.EmailVerified),
			Phone:         claims.PhoneNumber,
			PhoneVerified: boolToInt(claims.PhoneVerified),
			LinkedAt:      time.Now(),
		}); err != nil {
			if !isDuplicateKeyError(err) {
				fl.result.WithLabelValues("identity_insert_fail").Inc()
				o.Error("OIDC "+fl.logName+": insert identity failed",
					zap.String("trace_id", vi.traceID), zap.Error(err))
				httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
				return
			}
			recovered := o.recoverFromIdentityRace(ctx, claims, vi.state, sessResp, issueReq, err, fl.eventFail)
			if recovered == nil {
				fl.result.WithLabelValues("identity_insert_fail").Inc()
				o.writeAudit(sessResp.UID, fl.eventFail, vi.state, "identity insert race unrecovered")
				httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
				return
			}
			// **必须改用赢家会话。** 输家(ghost)的 uid 没有 identity 行,把它的
			// token 交给客户端等于发了一个孤立账号:所有依赖 identity 的功能对
			// 那个会话静默空转,而审计会把"登录成功"记在 ghost 头上。
			// callback 路径一直是这么做的,两个 exchange 端点曾经漏了这一行。
			sessResp = recovered
			fl.result.WithLabelValues("race_recovered").Inc()
			// 竞态输家的 user 行是 ghost(identity 归赢家,这个 uid 谁也登不上),
			// 不该占初始 Space 的一个席位 —— 尤其在 max_users 卡着的部署里,一个
			// ghost 就挤掉一个真人。赢家账号在它自己那条请求里已经走过一次自动加入。
			autoJoinUID = ""
		}
	}

	// 建号成功 → 自动加入运维配置的初始 Space(task oidc-auto-join-initial-space)。
	//
	// 两个 exchange 端点与 callback / bind create 一样是建号入口:经它们建出来的
	// 账号如果不进 Space,一样会卡在 POST /v1/integrations/oidc/exchange 的成员校验上,
	// 也就是这个功能要消灭的那个死角。同步执行、失败不影响本次响应,详见函数注释。
	if autoJoinUID != "" {
		o.autoJoinInitialSpace(autoJoinUID)
	}

	o.writeAudit(sessResp.UID, fl.eventOK, vi.state, vi.auditDetail)
	fl.result.WithLabelValues("ok").Inc()

	// 与 /bind/create 返回形状对齐:status / uid / login_resp(JSON 字符串,
	// 客户端直接解出 token 字段)。
	c.JSON(http.StatusOK, map[string]interface{}{
		"status":     "ok",
		"uid":        sessResp.UID,
		"login_resp": sessResp.LoginRespJSON,
	})
}
