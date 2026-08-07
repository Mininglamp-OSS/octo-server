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

// ScanLoginPollSecretHeader 是轮询方出示密钥的**首选**通道。
//
// 走 header 而非 query：密钥明文若进 URL，会被 nginx/ingress access log、CDN/WAF 日志、
// APM trace span 与浏览器历史逐字记录 —— 那正好抵消「Redis 只存摘要」想防的泄露面。
//
// 注意 header 通道**目前只在同源部署可用**：octo-lib 的 CORSMiddleware
// （pkg/wkhttp/http.go）把 Access-Control-Allow-Headers 写死成一份不含本头的清单，且对
// OPTIONS 直接 AbortWithStatus(204)，headers 当场 flush —— octo-server 侧无论把中间件挂
// 在它之前还是之后都改不动。自定义头会让轮询变成非简单请求，跨源浏览器预检被拒后**真正
// 的 GET 根本发不出去**，连「剥字段降级」都轮不到。受影响的是 Tauri/Electron 正式包
// （apps/web/src/apiURL.ts 对桌面端返回绝对地址直连），Web 走相对路径不受影响。
const ScanLoginPollSecretHeader = "X-Scan-Poll-Secret"

// scanLoginPollSecretQuery 是跨源客户端的**过渡**通道。
//
// 存在的唯一理由是上面那条 CORS 限制。它把明文放回 URL，因此劣于 header —— 但两害相权：
// 桌面端要么用它、要么扫码登录完全不可用。
//
// SUNSET：octo-lib 把 X-Scan-Poll-Secret 加进 Access-Control-Allow-Headers 并发版、
// 本仓 bump 依赖之后，删掉这个 query 分支与 octo-web 侧对应的兜底。
const scanLoginPollSecretQuery = "poll_secret"

// ScanLoginAuthCodeTTL 是扫码登录授权码的存活时间。
//
// authCode 可被 POST /v1/user/login_authcode/{code} 直接兑换成 scaner 的完整 token，
// 所以这个 TTL 就是「凭据一旦外泄的可利用窗口」。原值 10 分钟远超实际所需。
//
// 定义在 modules/user 而非 modules/qrcode：grantLogin（本包）确认时要按同一档位给
// authCode 续期（见 P1-3），而 qrcode 已经 import user，反向 import 会成环。
const ScanLoginAuthCodeTTL = 5 * time.Minute

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
// 已扫码的用户甩掉。
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
// （1000 req/min/IP）仍在外层兜底。大规模 NAT 部署可直接上调。
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
// modules/qrcode.handleScanLogin、grantLogin。白名单反过来：新字段默认不下发，要下发
// 必须有人显式加进来并想清楚。
var scanLoginPublicDataKeys = map[string]struct{}{
	"status": {}, // 扫码进度，前端状态机必需
	"app_id": {}, // 常量标识，无信息量
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
// Fail-closed：store 读失败、key 不存在、presented 为空，一律 false。err 单独返回供
// 调用方记日志，但不影响判定结果 —— 读不到就是不放行。
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
