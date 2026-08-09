package user

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"go.uber.org/zap"
)

// scanLoginPollSecretPrefix 是扫码登录轮询密钥的 Redis 前缀。
//
// 该前缀在 octo-server 本地定义（而非 octo-lib common.*CachePrefix）：octo-lib 是
// 外部模块，加常量需要先发版；本 key 的生命周期完全由 modules/user 掌握。
const (
	scanLoginPollSecretPrefix = "scanlogin:poll:"
	scanLoginStatusDisabled   = "disabled"
)

// scanLoginPollSecretQuery 是轮询方出示密钥的 query 参数名。
//
// 曾经改成自定义请求头 X-Scan-Poll-Secret，想让明文不进 access log —— 后来退回来了。
// 这里记下取舍，免得有人再走一遍：
//
// 收益侧很窄。能读 nginx/APM 日志的是运维，同一批人有 Redis 权限，可以直接
// GET qrcode:{uuid} 读出 auth_code，甚至直接读 token 缓存 —— 自定义头挡不住任何一个
// 本来就能轻易接管账号的人。而这枚密钥 12 分钟过期、兑换即吊销，且单独毫无用处
// （必须有一个正在进行中的扫码流程同时存在）。
//
// 成本侧却很实。自定义头会让轮询变成非简单请求，而 octo-lib 的 CORSMiddleware 把
// Access-Control-Allow-Headers 写死、且对 OPTIONS 立即 AbortWithStatus(204) 并 flush
// —— 下游无论把中间件挂在它之前还是之后都改不动。跨源预检被拒后**真正的 GET 根本发不
// 出去**，Tauri/Electron 正式包（apps/web/src/apiURL.ts 走绝对地址）扫码登录直接不可用。
// 要修就得 octo-lib 改白名单 → 发版 → 本仓 bump 依赖，再加一套 header/query 双通道过渡。
//
// 真要防日志泄露，正确的位置是日志层脱敏（nginx map 重写 $request、APM 的 URL
// scrubbing），一次配置覆盖所有敏感 query 参数，而不是每加一个就动一次共享库的 CORS。
const scanLoginPollSecretQuery = "poll_secret"

// ScanLoginAuthCodeTTL 是扫码登录授权码的存活时间。
//
// authCode 在 grantLogin 明确确认后才会进入可兑换命名空间，并可被 POST
// /v1/user/login_authcode/{code} 兑换成 scanner 的完整 token，所以这个 TTL 就是
// 「已确认凭据一旦外泄的可利用窗口」。原值 10 分钟远超实际所需。
//
// 定义在 modules/user 而非 modules/qrcode：grantLogin（本包）确认时用它设置 ready
// authorization 的完整窗口，而 qrcode 已经 import user，反向 import 会成环。
const ScanLoginAuthCodeTTL = 5 * time.Minute

// ScanLoginConfirmWindow 是「二维码已被扫描 / 已授权」状态的存活时间，也就是用户可以
// 停在确认页、以及浏览器完成兑换的时间上限。
//
// 单独命名而不是继续散落 time.Minute*5：scanLoginPollSecretTTL 的推导把这个量当作
// 一个三项累加里的两项在用，写成字面量就意味着注释里的算术、守卫测试里的建模、以及
// 代码实际行为是三份互不相干的东西。两个写入方（modules/qrcode.handleScanLogin 与
// grantLogin）现在都引用它。
const ScanLoginConfirmWindow = 5 * time.Minute

// scanLoginPollSecretTTL 是轮询密钥的存活时间。
//
// 必须覆盖「二维码状态还可能被读到」的最坏路径，否则用户扫得晚一点就会拿不到
// auth_code。该路径是三段累加：
//
//	mint 后 60s 内扫码（getLoginUUID 写 qrcode:{uuid} 的初始 TTL）
//	+ ScanLoginConfirmWindow（handleScanLogin 续期，用户停在确认页的上限）
//	+ ScanLoginConfirmWindow（grantLogin 确认后再续，浏览器兑换前的窗口）
//	= 60s + 2×5min = 660s
//
// 取 12 分钟给 60s 余量，避免 Redis 与应用间的时钟漂移或调用延迟正好卡在边界上把
// 已扫码的用户甩掉。
//
// 长活本身不额外授权：密钥必须与已确认 auth_code 配对才能兑换，qrcode 状态一过期
// 也无内容可读；loginWithAuthCode 兑换成功后会立刻 deleteScanLoginPollSecret，常态下
// 活不满 TTL。
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
//
// 档位刻意留一个数量级的余量，而不是贴着上面那笔估算走：初版取 120/600，正好是估算值
// 的 83%/100% —— 讲着「要留余量」却没留。100 人同时打开登录页（重启后、上班高峰）会
// 直接吃满 burst，而失败面是通用的 i18n rate.limited，运维根本归因不到这两个桶。
//
// 更贴切的建模其实是并发上限而非请求速率（loginstatus 是 10s 长轮询，一个请求占的是
// 连接而不是一次调用），但那要换一套中间件；在此之前用宽档位换「绝不误伤」。
const (
	defaultScanLoginUUIDRateLimitRPS     = 1200.0 / 60 // 1200 req/min
	defaultScanLoginUUIDRateLimitBurst   = 600
	defaultScanLoginStatusRateLimitRPS   = 6000.0 / 60 // 6000 req/min
	defaultScanLoginStatusRateLimitBurst = 3000
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

func scanLoginStatusIs(model *common.QRCodeModel, status string) bool {
	if model == nil {
		return false
	}
	return fmt.Sprint(model.Data["status"]) == status
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

// ensureScanLoginStatus 保证下发给未授权轮询方的 payload 里一定有 status。
//
// 白名单的默认行为是「新字段不下发」，这对凭据是对的、对 status 是错的：一旦过滤后
// 的 map 里没有 status（Data 为 nil，或将来某个写入方构造出一份键全被丢掉的 payload），
// 前端拿到的 result.status 是 undefined → loginStatus 变 undefined → 状态机匹配不到
// 任何分支 → 轮询永久停摆。这正是 respondStatus 当初要消除的故障。
//
// 回落顺序：过滤结果里的 status → 原始 Data 里的 status → expired。
func ensureScanLoginStatus(filtered map[string]interface{}, original map[string]interface{}) map[string]interface{} {
	if filtered == nil {
		filtered = map[string]interface{}{}
	}
	if _, ok := filtered["status"]; ok {
		return filtered
	}
	if v, ok := original["status"]; ok {
		filtered["status"] = v
		return filtered
	}
	filtered["status"] = common.ScanLoginStatusExpired
	return filtered
}
