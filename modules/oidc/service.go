package oidc

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 业务错误。callback handler 会把这些错误翻成不同的 HTTP 状态/前端提示。
var (
	// ErrUnknownUser claims 未命中任何已存在的 dmwork 账号,且 AllowNewUser=false。
	ErrUnknownUser = errors.New("oidc: claims do not match any existing dmwork user")
	// ErrConflictNeedManual 同邮箱/手机号在 dmwork 端命中多条用户,需人工合并。
	ErrConflictNeedManual = errors.New("oidc: multiple dmwork users matched, manual link required")
	// ErrEmailNotVerified IdP 返回的邮箱未验证,且配置要求验证后才能自动绑定/创建。
	ErrEmailNotVerified = errors.New("oidc: email not verified by IdP")
)

// ResolveResult ResolveOrLink 的结果(UID + 是否新创建)。
type ResolveResult struct {
	UID   string
	IsNew bool
}

// IssueSessionReq 把 ResolveOrLink 的输出 + 请求级设备信息透给 IssueSession。
//
// 字段命名与 user.ExternalLoginReq 平行,但保持独立类型避免 oidc 直接依赖 user 类型。
type IssueSessionReq struct {
	UID        string
	CreateUser bool
	Name       string
	Email      string
	Phone      string
	Zone       string
	DeviceFlag uint8
	DeviceID   string
	DeviceName string
	DeviceMod  string
	PublicIP   string

	// TrustedSSOCreate 透传到 user.ExternalLoginReq.TrustedSSOCreate,
	// 让可信 IdP 触发的新建用户路径绕过 user 模块的 register.off 全局开关。
	//
	// 仅在 CreateUser=true 时有意义。callback `res.IsNew=true` 与 /bind/create
	// 两条入口显式置 true(代表 IssuerAllowlist 已经过),其他路径(verify→confirm
	// 绑定老用户)留 false —— 与 CreateUser 本身的语义对齐:不建用户的请求不该
	// 表达"信任创建"语义。
	TrustedSSOCreate bool
}

// IssueSessionResp 会话签发结果。LoginRespJSON 直接塞 ThirdAuthcode Redis,
// 前端短码轮询取走;调用方不解析其内容。
type IssueSessionResp struct {
	UID           string
	IsNewUser     bool
	LoginRespJSON string
}

// userLookup oidc service 对 user 模块依赖的最小接口。
//
// 生产环境用 user.IService + oidc.DB 适配器实现;测试用 fakeUserLookup。
// 接口在 service 包内定义,符合 "Accept interfaces, return structs"。
type userLookup interface {
	UIDsByEmail(email string) ([]string, error)
	UIDsByPhone(zone, phone string) ([]string, error)
	IssueSession(ctx context.Context, req IssueSessionReq) (*IssueSessionResp, error)
}

// identityStore oidc service 对 oidc DB 的最小读写接口(仅 ResolveOrLink 用到)。
//
// GetByUIDIssuer/UpdateSubject 是上游轮换 sub 时的恢复路径:uk_uid_issuer 规定
// 一个 uid 在一个 issuer 下只能有一行,所以换了 sub 之后必须 UPDATE 既有行,
// 盲 INSERT 会撞 1062 把该用户永久锁死(见 linkSingleMatch)。
type identityStore interface {
	Get(issuer, subject string) (*IdentityModel, error)
	// GetByUIDIssuer 返回该 (uid, issuer) 下的绑定行;无行返回 (nil, nil)。
	//
	// 列 collation 是 ci,所以返回行的 uid/issuer **可能只是折叠相等**。
	// 调用方必须自己逐字节复核,见 linkSingleMatch。
	GetByUIDIssuer(uid, issuer string) (*IdentityModel, error)
	Insert(m *IdentityModel) error
	// UpdateSubject 只改 subject 一列。claims(email/phone/last_login_at)由调用方
	// 随后的 Get+UpdateLogin 刷新,这里重复写一遍只会制造两处会漂移的真相。
	UpdateSubject(id int64, subject string) error
	UpdateLogin(id int64, email string, emailVerified int, phone string, phoneVerified int) error
}

// Service OIDC 业务编排层。
//
// 职责:
//  1. ResolveOrLink — 把 IdP claims 解析为 dmwork uid(必要时建账或绑定历史账号)
//  2. IssueSession  — 调 user.IService.LoginByExternalIdentity 签发会话
type Service struct {
	cfg   ProviderConfig
	store identityStore
	users userLookup
	now   func() time.Time
}

// newService 构造 Service,接受小接口 store/users 注入。
//
// 生产路径(NewService in user_adapter.go)和测试路径都走这个构造函数,
// 测试注入 fake store/users,生产注入 DB/userAdapter。
func newService(cfg ProviderConfig, store identityStore, users userLookup) *Service {
	return &Service{
		cfg:   cfg,
		store: store,
		users: users,
		now:   time.Now,
	}
}

// ResolveOrLink Issue #1120 的历史账号绑定矩阵。
//
// 规则(按顺序短路):
//
//   - 1. (issuer, sub) 命中已绑定 identity 行 → 返回原 UID(场景 3:重复登录)
//
//   - 2. AutoLinkByEmail=true && claims.email_verified:
//     a. user.email 命中 1 条 → 写绑定 → 返回该 UID(场景 3:首次绑历史账号)
//     b. user.email 命中多条 → ErrConflictNeedManual(场景 4:脏数据冲突)
//     c. 未命中 → 走 step 3
//
//   - 3. user.phone 同上(命中 1 / 多条 / 未命中)
//
//   - 4. AllowNewUser=true → 不写本地 user 表,只返回 IsNew=true 让 IssueSession 创建
//     (避免 service 层直接持 user.IService 写权限,职责更清)
//
//   - 5. AllowNewUser=false → ErrUnknownUser
//
// 返回 IsNew=true 时 UID 为空,由 IssueSession 通过 user.IService 创建并回填。
func (s *Service) ResolveOrLink(ctx context.Context, claims *IDTokenClaims) (*ResolveResult, error) {
	// 可落库性守卫:非空 + 不超列宽 + subject 形态。见 identity_bounds.go ——
	// 这里是 callback 与两个 exchange 端点的共同入口,且在 IssueSession 之前,
	// 所以是"副作用之前"能覆盖到全部协议的那个点。
	if err := requireStorableIdentity(claims); err != nil {
		return nil, fmt.Errorf("oidc: ResolveOrLink: %w", err)
	}

	// 1. 已绑定
	if existing, err := s.store.Get(claims.Issuer, claims.Subject); err != nil {
		return nil, fmt.Errorf("oidc: ResolveOrLink: query identity: %w", err)
	} else if existing != nil {
		return &ResolveResult{UID: existing.UID, IsNew: false}, nil
	}

	// 2. 邮箱自动绑定
	//
	// RequireEmailVerified=true 且 email 未验证时,只跳过邮箱绑定分支——
	// 不 return,让 step 3(phone)和 step 4(AllowNewUser)继续有机会命中。
	// 之前直接 return ErrEmailNotVerified 会短路整条矩阵,导致"邮箱未验证但
	// 手机可绑"和"邮箱未验证但允许新建用户"两个合法路径全被堵死。
	emailLinkable := s.cfg.AutoLinkByEmail && claims.Email != "" &&
		(!s.cfg.RequireEmailVerified || claims.EmailVerified)
	if emailLinkable {
		uids, err := s.users.UIDsByEmail(claims.Email)
		if err != nil {
			return nil, fmt.Errorf("oidc: ResolveOrLink: lookup email: %w", err)
		}
		if uid, err := s.linkSingleMatch(uids, claims); err != nil || uid != "" {
			if err != nil {
				return nil, err
			}
			return &ResolveResult{UID: uid, IsNew: false}, nil
		}
	}

	// 3. 手机号自动绑定。
	//
	// PhoneVerified 一律强制 true(没设 RequirePhoneVerified 配置):手机号
	// 在国内是强账号载体,被劫持/未验证就绑可能直接接管账户,默认收紧。
	if s.cfg.AutoLinkByPhone && claims.PhoneNumber != "" && claims.PhoneVerified {
		uids, err := s.users.UIDsByPhone(extractZone(claims.PhoneNumber), extractPhone(claims.PhoneNumber))
		if err != nil {
			return nil, fmt.Errorf("oidc: ResolveOrLink: lookup phone: %w", err)
		}
		if uid, err := s.linkSingleMatch(uids, claims); err != nil || uid != "" {
			if err != nil {
				return nil, err
			}
			return &ResolveResult{UID: uid, IsNew: false}, nil
		}
	}

	// 4/5. 走新建 or 拒绝
	if !s.cfg.AllowNewUser {
		return nil, ErrUnknownUser
	}
	return &ResolveResult{IsNew: true}, nil
}

// IssueSession 委托给 user.IService.LoginByExternalIdentity 签发会话。
//
// 只是个薄壳:负责把 oidc IssueSessionReq 透传到 userLookup,真正逻辑(token /
// IM token / 创建用户)在 user 模块。返回的 LoginRespJSON 由 callback 直接落
// ThirdAuthcode Redis,前端短码轮询取走。
func (s *Service) IssueSession(ctx context.Context, req IssueSessionReq) (*IssueSessionResp, error) {
	if req.UID == "" && !req.CreateUser {
		return nil, fmt.Errorf("oidc: IssueSession: UID required when CreateUser=false")
	}
	resp, err := s.users.IssueSession(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("oidc: issue session: %w", err)
	}
	return resp, nil
}

// linkSingleMatch 把唯一匹配的 uid 绑到 claims 上;多匹配返回冲突;无匹配返回("", nil)走下一规则。
//
// 唯一匹配分支不能盲 INSERT —— uk_uid_issuer 规定一个 uid 在一个 issuer 下只有
// 一行。上游把某用户的 sub 换掉后,ResolveOrLink step 1 按新 sub 查不到,
// email/phone 又匹到同一个 uid,盲 INSERT 于是撞 1062:整条 callback 失败,重试
// 完全幂等,用户永久登不进来,只能人工删行。所以先查 (uid, issuer):
//
//	无行          → 首次绑定,照原路 INSERT
//	有行且 sub 相同 → 幂等返回(理论上 step 1 就该命中)
//	有行且 sub 不同 → sub 轮换,UPDATE 既有行的 subject
//
// UPDATE 对 uk_issuer_subject 是安全的:step 1 的 Get(issuer, claims.Subject)
// 已经证明新 sub 当前没绑在任何 uid 上(命中就从 fast path 返回了)。信任边界
// 沿用已经授权 auto-link 的那一条(verified email / verified phone + 本模块配置
// 开关),不新开口子。
func (s *Service) linkSingleMatch(uids []string, claims *IDTokenClaims) (string, error) {
	switch len(uids) {
	case 0:
		return "", nil
	case 1:
		uid := uids[0]
		existing, err := s.store.GetByUIDIssuer(uid, claims.Issuer)
		if err != nil {
			return "", fmt.Errorf("oidc: link identity: query existing binding: %w", err)
		}
		if existing == nil {
			return s.insertLink(uid, claims)
		}
		// ci collation 下 `WHERE uid=? AND issuer=?` 会命中只差大小写的行。对一行
		// issuer 字节不等的记录 UPDATE subject,等于把**另一个身份命名空间**的绑定
		// 改写到当前 sub 上 —— 账号接管,不是脏数据。所以折叠行一律响亮拒绝。
		//
		// 不能当作"没查到"继续 INSERT:那会再次撞 ci 唯一键,回到 1062 死循环
		// (正是 ErrIdentityCaseCollision 注释里那个坑)。
		if !identityBindingMatches(existing, uid, claims.Issuer) {
			metricIdentitySubjectRotatedTotal.WithLabelValues("conflict_manual").Inc()
			return "", fmt.Errorf("oidc: link identity: existing binding differs only by case "+
				"folding, refusing to rewrite another identity namespace: %w", ErrConflictNeedManual)
		}
		if identitySubjectMatches(claims.Subject, existing.Subject) {
			return uid, nil
		}
		return s.rotateSubject(existing.Id, uid, claims.Subject, "rotated")
	default:
		return "", ErrConflictNeedManual
	}
}

// insertLink 写首次绑定行,并把 1062 翻成可区分的业务结果。
//
// GetByUIDIssuer 与 Insert 之间没有锁,两种并发都会以 1062 回来:
//
//	(uid, issuer) 被并发插了一行 → 重查:同 sub 幂等返回 / 不同 sub 走轮换恢复
//	(issuer, sub) 被别的 uid 抢绑 → 重查查不到本 uid 的行 → ErrConflictNeedManual
//
// 两者都绝不裸抛 1062:上层把 ErrConflictNeedManual 路由到人工绑定流程,比
// 500 + 永久重试强得多。也绝不改写别的 uid 对该 sub 的所有权。
func (s *Service) insertLink(uid string, claims *IDTokenClaims) (string, error) {
	err := s.store.Insert(&IdentityModel{
		UID:           uid,
		Issuer:        claims.Issuer,
		Subject:       claims.Subject,
		Email:         claims.Email,
		EmailVerified: boolToInt(claims.EmailVerified),
		Phone:         claims.PhoneNumber,
		PhoneVerified: boolToInt(claims.PhoneVerified),
		LinkedAt:      s.now(),
	})
	if err == nil {
		return uid, nil
	}
	if !isDuplicateKeyError(err) {
		return "", fmt.Errorf("oidc: link identity: %w", err)
	}
	existing, qerr := s.store.GetByUIDIssuer(uid, claims.Issuer)
	if qerr != nil {
		return "", fmt.Errorf("oidc: link identity: re-query after duplicate key: %w", qerr)
	}
	// 折叠行与"查不到"在这里同样处理:两者都意味着这一行不是本 (uid, issuer)
	// 字节意义上的绑定,碰 subject 就是接管别人的身份。
	if !identityBindingMatches(existing, uid, claims.Issuer) {
		metricIdentitySubjectRotatedTotal.WithLabelValues("conflict_manual").Inc()
		return "", fmt.Errorf("oidc: link identity: duplicate key and no binding owned by this "+
			"user: %w", ErrConflictNeedManual)
	}
	if identitySubjectMatches(claims.Subject, existing.Subject) {
		// 并发赢家写的就是同一个绑定,幂等。
		return uid, nil
	}
	return s.rotateSubject(existing.Id, uid, claims.Subject, "race_recovered")
}

// rotateSubject 把既有绑定行的 subject 换成本次已验证的新 sub。
//
// 1062 只可能来自 uk_issuer_subject —— 新 sub 在 step 1 查空之后被别的 uid 抢绑。
// 这种情况下所有权属于对方,返回 ErrConflictNeedManual 交人工,不覆盖。
//
// outcome 区分正常轮换与 INSERT 竞态后的恢复,两条曲线在运维上问的不是同一个问题。
func (s *Service) rotateSubject(id int64, uid, subject, outcome string) (string, error) {
	if err := s.store.UpdateSubject(id, subject); err != nil {
		if isDuplicateKeyError(err) {
			metricIdentitySubjectRotatedTotal.WithLabelValues("conflict_manual").Inc()
			return "", fmt.Errorf("oidc: rotate identity subject: subject already owned by "+
				"another user: %w", ErrConflictNeedManual)
		}
		return "", fmt.Errorf("oidc: rotate identity subject: %w", err)
	}
	metricIdentitySubjectRotatedTotal.WithLabelValues(outcome).Inc()
	return uid, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// mainlandMobileRe 中国大陆手机号号码体(不含国家码):1 + [3-9] + 9 位数字。
//
// 只在**已经剥掉大陆国家码之后**用它校验号码体,不用它去判断一个裸号是不是
// 大陆号 —— 后者做不到,见 normalizePhone 的注释。
var mainlandMobileRe = regexp.MustCompile(`^1[3-9]\d{9}$`)

// phoneSeparators 人工录入常见的分隔符,归一化前先剔除。
const phoneSeparators = " \t-()"

// normalizePhone 把上游 phone_number 归一化成 dmwork 的 (zone, phone) 形态。
//
// 支持的输入(全部映射到 zone="0086"):
//
//	+8613812345678 / 008613812345678 / 8613812345678 / 13812345678
//	以及上述形态中夹带空格、'-'、括号的版本
//
// 设计取舍 —— 为什么不做通用 E.164 解析,也不推断裸号:
//
//	E.164 的国家码是 1~3 位变长,纯语法切不开(+4670…既可以是 +46 70… 也可以
//	是 +467 0…),要切必须内置一张国家码表。而猜错国家码的后果是把号码存成另一
//	个国家的另一个号:用户从此收不到验证码、找不回账号,而且错误不可见 —— 库里
//	那一行看起来完全正常。
//
//	裸号(不带国家码)同样不能推断,理由是具体的:北美编号计划的号码是 `1` +
//	三位区号,区号首位取 [2-9],于是**约 7/8 的北美号码与中国号段在裸号形态下
//	完全同形** —— `13861234567` 既可以是 +1 386-123-4567,也是一个真实的中国
//	138 号段号码。把前者按后者存下去,存进去的是某个中国人的号码。
//
//	早先版本按 `^1[3-9]\d{9}$` 推断裸号,依据是厂商手册 userinfo 的一个示例;
//	而那个示例值 `11136618971` 以 `111` 开头,根本不是合法的中国号段。也就是说
//	我们从未见过上游发出合法的裸中国号,那个推断建立在一个假造的示例上。
//	等实测确认上游的真实形态后再按实测加回来。
//
//	所以这里只归一化能**确定**的形态,其余一律返回空对,由调用方按"这个用户
//	没有可用手机号"处理(它们已经都能处理空值)。
//
// 海外号(该客户在多个国家有员工)因此仍然不会被自动写入。这是有意的:
// 目前 dmwork 侧只有 0086 的短信通道被验证过,存一个发不出验证码的号码
// 比不存更糟 —— 它会让"用手机号找回账号"这条路看起来可用但实际失败。
//
// 两个返回值同进同退:要么都非空,要么都空。调用方(bind_service.go 用
// extractPhone 判定可用性,service.go 把两者一起传给 UIDsByPhone)依赖这个不变量。
func normalizePhone(raw string) (zone, phone string) {
	s := strings.Trim(raw, phoneSeparators)
	for _, sep := range phoneSeparators {
		s = strings.ReplaceAll(s, string(sep), "")
	}
	if s == "" {
		return "", ""
	}
	// 大陆国家码的三种写法。三者互不为前缀(+86 / 0086 / 86),顺序无关。
	for _, cc := range []string{"+86", "0086", "86"} {
		if rest, ok := strings.CutPrefix(s, cc); ok {
			if mainlandMobileRe.MatchString(rest) {
				return mainlandZone, rest
			}
			// 明确声明了大陆国家码,号码体却不是合法号段 —— 上游数据有问题,
			// 不要退回去把整串当裸号再试一次(那会把 "+8611136618971" 这种
			// 脏数据洗成一个看起来合法的号码)。
			return "", ""
		}
	}
	// 裸号不推断 —— 见函数注释里的 NANP 同形说明。
	return "", ""
}

// mainlandZone dmwork 的区号形态是 "00" + 国家码。
const mainlandZone = "0086"

// extractZone 返回归一化后的区号;无法确定归属时返回空(调用方按不绑定处理)。
func extractZone(phone string) string {
	zone, _ := normalizePhone(phone)
	return zone
}

// extractPhone 返回归一化后的号码体(不含国家码);无法确定归属时返回空。
func extractPhone(phone string) string {
	_, p := normalizePhone(phone)
	return p
}
