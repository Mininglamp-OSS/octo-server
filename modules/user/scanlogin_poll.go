package user

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"go.uber.org/zap"
)

// scanLoginPollSecretPrefix 是扫码登录轮询密钥的 Redis 前缀。
//
// 该前缀在 octo-server 本地定义（而非 octo-lib common.*CachePrefix）：octo-lib 是
// 外部模块，加常量需要先发版；本 key 的生命周期完全由 modules/user 掌握。
const scanLoginPollSecretPrefix = "scanlogin:poll:"

// ScanLoginPollSecretHeader 是轮询方出示密钥用的请求头。
//
// 走 header 而非 query：密钥的明文若进 URL，会被 nginx/ingress access log、CDN/WAF
// 日志、APM trace span 与浏览器历史逐字记录 —— 那正好抵消了「Redis 只存摘要」想防的
// 泄露面。header 不进请求行，也就不进这些日志的默认字段。
const ScanLoginPollSecretHeader = "X-Scan-Poll-Secret"

// scanLoginPollSecretTTL 是轮询密钥的存活时间。
//
// 必须覆盖「二维码状态还可能被读到」的最坏路径，否则用户扫得晚一点就会拿不到
// auth_code。该路径是三段累加：
//
//	mint 后 60s 内扫码（getLoginUUID 写 qrcode:{uuid} 的初始 TTL）
//	+ 300s   （handleScanLogin 把 qrcode 续到 5min，用户在确认页停顿的上限）
//	+ 300s   （grantLogin 确认后再续 5min，浏览器兑换前的窗口）
//	= 660s
//
// 取 12 分钟给 60s 余量，避免 Redis 与应用间的时钟漂移或调用延迟正好卡在边界上把
// 已扫码的用户甩掉（上一版取 6 分钟且零余量，扫码发生在第 59 秒时必然踩中）。
//
// 长活本身不额外授权：密钥只是读取凭据的门票，qrcode 状态一过期就无内容可读；且
// loginWithAuthCode 兑换成功后会立刻 deleteScanLoginPollSecret，常态下活不满 TTL。
const scanLoginPollSecretTTL = 12 * time.Minute

// 扫码登录两个未认证端点的默认限流档位（可经 DM_API_SCANLOGIN_*_RATELIMIT_{RPS,BURST}
// 覆盖，见 api.go 的挂载点）。
//
// 这两个值是「防批量滥用的地板」，不是精细控制，所以给得很松。定档依据：
//   - qrcode:{uuid} 初始 TTL 只有 60s，所以每个停在登录页的浏览器约每分钟重铸一次
//     uuid —— 一个 100 人的办公室就是 ~100 次/分，全部来自同一个出口 IP。
//   - 反向代理若没配 X-Real-Ip / X-Forwarded-For，octo-lib 的 getClientIP 会回落到
//     RemoteAddr，即 nginx 自己的地址 —— 整个部署塌进同一个桶。
//
// 卡死的后果是「一层楼扫不了码」，比放过一些扫描流量严重得多。全局 RateLimitMiddleware
// （1000 req/min/IP）仍在外层兜底，所以这里没必要再收紧。大规模 NAT 部署可直接上调。
const (
	defaultScanLoginUUIDRateLimitRPS     = 120.0 / 60 // 120 req/min
	defaultScanLoginUUIDRateLimitBurst   = 60
	defaultScanLoginStatusRateLimitRPS   = 600.0 / 60 // 600 req/min
	defaultScanLoginStatusRateLimitBurst = 300
)

// scanLoginPublicDataKeys 是 qrcode:{uuid} 状态里允许回给**任意**匿名轮询方的键。
//
// 这里刻意用白名单而不是「敏感键黑名单」：黑名单在有人往 payload 里加字段时默认放行
// （fail-open），而这个 payload 的写入方分散在三处 —— getLoginUUID、
// modules/qrcode.handleScanLogin、grantLogin。上一版的黑名单加一个只读 grantLogin 的
// 守卫测试，handleScanLogin 里新增的字段（扫码者昵称/头像/手机号等）会直接漏给攻击者。
// 白名单反过来：新字段默认不下发，要下发必须有人显式把它加进来并想清楚。
var scanLoginPublicDataKeys = map[string]struct{}{
	"status": {}, // 扫码进度，前端状态机必需
	"app_id": {}, // 常量标识，无信息量
}

// scanLoginOriginPrefix 记录「是谁请求了这个二维码」，供手机确认页展示。
//
// 与既有的 common.DeviceCacheUUIDPrefix 分开存：那份记录只在 device_id/name/model
// 三者齐全时才写，且被 loginWithAuthCode 用于登记设备；这里需要的是**无条件可得**的
// 来源上下文（尤其是 IP），不能因为客户端没上报设备字段就缺失。
const scanLoginOriginPrefix = "scanlogin:origin:"

// ScanLoginOrigin 是发起扫码登录的那一端的可展示上下文。
//
// 存在的理由：poll_secret 挡不住 QRLJacking —— 攻击者自己调 loginuuid，密钥自然也在
// 他手里。这条攻击链的唯一断点在**确认环节**：受害者必须能看出「请求登录的不是我的
// 设备」。服务端能做的是把判断依据备齐并下发给手机；真正生效还需要 iOS/Android 在
// 确认弹窗里渲染它。在那之前，这个结构体只是把数据准备好。
type ScanLoginOrigin struct {
	IP          string `json:"ip"`
	DeviceName  string `json:"device_name,omitempty"`
	DeviceModel string `json:"device_model,omitempty"`
	UserAgent   string `json:"user_agent,omitempty"`
}

func scanLoginOriginKey(uuid string) string {
	return fmt.Sprintf("%s%s", scanLoginOriginPrefix, uuid)
}

// storeScanLoginOrigin 写入来源上下文，与轮询密钥同寿命。
func storeScanLoginOrigin(store pollSecretStore, uuid string, origin ScanLoginOrigin) error {
	return store.SetAndExpire(scanLoginOriginKey(uuid), util.ToJson(origin), scanLoginPollSecretTTL)
}

// LoadScanLoginOrigin 读取来源上下文，供 modules/qrcode 在扫码时回给手机。
// 读不到时返回 nil（不是错误）—— 上下文缺失不应该阻断扫码。
func LoadScanLoginOrigin(store pollSecretStore, uuid string) (*ScanLoginOrigin, error) {
	raw, err := store.GetString(scanLoginOriginKey(uuid))
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	var origin ScanLoginOrigin
	if err := util.ReadJsonByByte([]byte(raw), &origin); err != nil {
		return nil, err
	}
	return &origin, nil
}

// pollSecretStore 是轮询密钥所需的最小存储能力，签名对齐 octo-lib 的 *redis.Conn。
//
// 抽成接口是为了让密钥的写入/比对/删除能在没有 Redis 的环境下做真实行为测试 ——
// 这是本模块唯一的安全闸门，只用源码字符串断言覆盖它是不够的（key 拼错、存了明文、
// 写读两侧 key 不一致这类错误都能一路绿灯上线）。
type pollSecretStore interface {
	SetAndExpire(key string, value interface{}, expire time.Duration) error
	GetString(key string) (string, error)
	Del(key string) error
}

// scanLoginPollSecretKey 拼装轮询密钥的 Redis key。
func scanLoginPollSecretKey(uuid string) string {
	return fmt.Sprintf("%s%s", scanLoginPollSecretPrefix, uuid)
}

// hashScanLoginPollSecret 返回密钥的 SHA-256 十六进制摘要。
//
// 存摘要而非明文：Redis 快照 / 慢查询日志 / 运维 KEYS 扫描都可能让明文外泄，而该
// 明文等价于「读取 auth_code 的权限」。摘要不可逆，泄露也无法用于轮询。
func hashScanLoginPollSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// mintScanLoginPollSecret 为 uuid 生成一枚轮询密钥，把摘要写入 store，返回明文。
//
// 明文只在 getLoginUUID 的 HTTP 响应体里回给申请方一次。**绝不能写进
// qrcode:{uuid} 的 payload** —— 那个 payload 正是 getloginStatus 要回显给匿名轮询方
// 的内容，把密钥放进去等于把门钥匙挂在门上。
func mintScanLoginPollSecret(store pollSecretStore, uuid string) (string, error) {
	secret := util.GenerUUID() // crypto/rand UUIDv4，122 bit
	if err := store.SetAndExpire(
		scanLoginPollSecretKey(uuid),
		hashScanLoginPollSecret(secret),
		scanLoginPollSecretTTL,
	); err != nil {
		return "", err
	}
	return secret, nil
}

// scanLoginPollSecretMatches 判断 presented 是否为 uuid 当初下发的轮询密钥。
//
// Fail-closed：store 读失败、key 不存在、presented 为空，一律 false。任何一处
// fail-open 都会让未持密钥的轮询方拿到 auth_code。err 单独返回供调用方记日志，
// 但不影响判定结果 —— 读不到就是不放行。
//
// 比对用 subtle.ConstantTimeCompare 而非 ==：摘要是定长十六进制串，逐字节短路比较会
// 在响应时间上泄露前缀匹配长度。
func scanLoginPollSecretMatches(store pollSecretStore, uuid string, presented string) (bool, error) {
	if uuid == "" || presented == "" {
		return false, nil
	}
	stored, err := store.GetString(scanLoginPollSecretKey(uuid))
	if err != nil {
		return false, err
	}
	if stored == "" {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(hashScanLoginPollSecret(presented))) == 1, nil
}

// deleteScanLoginPollSecret 在扫码登录兑换完成后清掉密钥。
//
// 与 loginWithAuthCode 删除 authcode:{code} 同理：登录已经完成，密钥再留着就只是一段
// 多余的可利用窗口 —— 期间任何拿到它的人仍可读出 uid/encrypt。
func deleteScanLoginPollSecret(store pollSecretStore, uuid string) error {
	return store.Del(scanLoginPollSecretKey(uuid))
}

// filterScanLoginPublicFields 返回 data 中仅保留 scanLoginPublicDataKeys 的浅拷贝。
//
// 返回拷贝而非原地删除：同一个 *common.QRCodeModel 会经长轮询 channel 广播，原地改会
// 污染持有正确密钥那一方的视图。
func filterScanLoginPublicFields(data map[string]interface{}) map[string]interface{} {
	if data == nil {
		return nil
	}
	out := make(map[string]interface{}, len(scanLoginPublicDataKeys))
	for k, v := range data {
		if _, ok := scanLoginPublicDataKeys[k]; ok {
			out[k] = v
		}
	}
	return out
}

// --- *User 上的薄封装：注入生产用的 Redis 连接并统一记日志 ---------------------

func (u *User) mintScanLoginPollSecret(uuid string) (string, error) {
	return mintScanLoginPollSecret(u.ctx.GetRedisConn(), uuid)
}

func (u *User) scanLoginPollSecretMatches(uuid string, presented string) bool {
	ok, err := scanLoginPollSecretMatches(u.ctx.GetRedisConn(), uuid, presented)
	if err != nil {
		// 读不到就当没有 —— 宁可让合法用户停在授权页重试，也不能放行匿名轮询。
		u.Error("读取扫码轮询密钥失败", zap.String("uuid", uuid), zap.Error(err))
		return false
	}
	return ok
}

func (u *User) deleteScanLoginPollSecret(uuid string) {
	if uuid == "" {
		return
	}
	if err := deleteScanLoginPollSecret(u.ctx.GetRedisConn(), uuid); err != nil {
		// 清理失败不该让登录失败：密钥仍会按 TTL 自然过期。
		u.Warn("清理扫码轮询密钥失败", zap.String("uuid", uuid), zap.Error(err))
	}
}

func (u *User) storeScanLoginOrigin(uuid string, origin ScanLoginOrigin) {
	if err := storeScanLoginOrigin(u.ctx.GetRedisConn(), uuid, origin); err != nil {
		// 上下文只用于确认页展示，写失败不阻断发码 —— 手机端拿不到就退化为不展示。
		u.Warn("记录扫码登录来源失败", zap.String("uuid", uuid), zap.Error(err))
	}
}
