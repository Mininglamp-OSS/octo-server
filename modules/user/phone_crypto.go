package user

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// 手机号(PII)加密说明 —— 第一阶(加密列 + 盲索引 + 后4位检索列,明文 phone 列暂留):
//
//   - 主密钥:环境变量 OCTO_PII_ENCRYPTION_SECRET(32 字节)。独立于
//     modules/usersecret 的 OCTO_USER_API_KEY_SECRET —— 手机号是用户 PII,
//     和"用户外部密钥别名"是不同的数据分类,密钥域必须隔离。
//   - 子密钥派生:HMAC-SHA256(masterKey, domain)，加密和盲索引各用独立 domain
//     派生互不相同的子密钥，避免同一子密钥同时用于两种密码学原语。
//   - AEAD:AES-256-GCM，密文 = nonce(12B) || ciphertext || tag(16B)，外层加
//     版本前缀 phoneCipherVersionPrefix，与 usersecret/crypto.go 同款约定。
//   - 盲索引:HMAC-SHA256(zone+"|"+phone) 的 hex，确定性、不可逆，用于等值检索
//     （注册查重/找回密码/OIDC 绑定定位账号），不需要解密即可查询。
//   - 手机号后4位（phoneLast4）是独立的低敏明文字段，只服务"输入后4位模糊检索"
//     这一种检索语义，盲索引做不到子串匹配。
//
// 主密钥缺失时，encryptPhone/PhoneBlindHash 返回 error；调用方按 usersecret 的
// 降级口径处理：写路径跳过三个新列（明文 phone 列仍照常写入，不阻断注册/建号），
// 读路径退回明文比较（见 db.go QueryByPhone）。
const (
	phoneCipherVersionPrefix = "enc:v1:"
	phoneEncryptionSecretEnv = "OCTO_PII_ENCRYPTION_SECRET"
	phoneEncryptDomain       = "octo-user-phone-cipher-v1"
	phoneHashDomain          = "octo-user-phone-hash-v1"
	// loginAccountHashDomain 登录日志里账号盲索引的派生域。与手机号盲索引分开是因为
	// 输入语义不同（这里是任意登录标识符：手机号 / 邮箱 / username），同主密钥派生
	// 不同子密钥，密码学上相互独立。
	loginAccountHashDomain = "octo-login-account-hash-v1"

	// phoneHashKeyVersion 盲索引的密钥版本，写进 phone_hash 值本身（`<版本>:<hex>`）。
	//
	// 为什么现在就带上版本号：盲索引是主密钥确定性派生的，轮换
	// OCTO_PII_ENCRYPTION_SECRET 会让全部存量 phone_hash 失效。如果值里没有版本标识，
	// 轮换时无法区分"这行是旧密钥算的"与"这行还没回填"，也无法在过渡期同时接受两种
	// 哈希 —— 那时再加版本号就需要一次全量数据迁移。现在加只是多两个字符。
	//
	// 轮换步骤（待真正需要时实施，当前为单密钥）：
	//  1. 新增 OCTO_PII_ENCRYPTION_SECRET_PREVIOUS = 旧密钥，OCTO_PII_ENCRYPTION_SECRET = 新密钥；
	//  2. 本常量 +1，读路径改为对 [当前版本, 上一版本] 各算一个候选哈希、用 IN 查询；
	//  3. 跑回填：按新密钥重算 phone_encrypted / phone_hash；
	//  4. 回填完成后摘掉 PREVIOUS 与读路径的多版本分支。
	// 密文侧的版本位由 phoneCipherVersionPrefix 承担，同理演进。
	phoneHashKeyVersion = "1"
)

// errPhoneSecretKeyUnset 主密钥缺失/长度非法。与 usersecret 同语义：部署配置问题，
// 调用方决定是否降级而不是让进程崩溃。
var errPhoneSecretKeyUnset = fmt.Errorf("%s must be exactly 32 bytes", phoneEncryptionSecretEnv)

// phoneMasterKey 读取并校验主密钥（32 字节）。
func phoneMasterKey() ([]byte, error) {
	secret := []byte(os.Getenv(phoneEncryptionSecretEnv))
	if len(secret) != 32 {
		return nil, errPhoneSecretKeyUnset
	}
	return secret, nil
}

// derivePhoneSubkey HMAC-SHA256(masterKey, domain) 派生子密钥。
func derivePhoneSubkey(master []byte, domain string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte(domain))
	return mac.Sum(nil)
}

// phoneCryptoInput 加密/盲索引统一输入：zone+phone 拼接，避免不同 zone 相同号码
// 产生相同哈希/密文输入。
func phoneCryptoInput(zone, phone string) string {
	return zone + "|" + phone
}

// phoneLast4 取手机号后4位供低敏模糊检索。
//
// 必须按 rune 而不是 byte 切片：本仓库对 phone 列没有任何格式校验
// （managerAddUserReq.checkAddUserReq 只查非空），列里历史上也出现过邮箱、
// `<原号>@<stamp>@delete` 墓碑等非号码内容。按 byte 切片会在多字节输入上切断
// UTF-8 序列，产出非法 UTF-8 写进 phone_last4 VARCHAR(4) 时 MySQL 直接报
// 1366 Incorrect string value，导致整个建号事务失败。与 modules/usersecret 的
// maskTail 同款做法。
func phoneLast4(phone string) string {
	r := []rune(phone)
	if len(r) <= 4 {
		return phone
	}
	return string(r[len(r)-4:])
}

// PhoneBlindHash 计算 zone+phone 的盲索引，格式为 `<密钥版本>:<HMAC-SHA256 hex>`。
// 导出供本包之外（如 modules/oidc 的账号定位查询）在不引入完整 AEAD 依赖的前提下
// 复用同一盲索引算法。主密钥缺失时返回 error，调用方应退回明文比较兜底，不能让查询
// 直接失败。版本前缀的用途见 phoneHashKeyVersion。
func PhoneBlindHash(zone, phone string) (string, error) {
	master, err := phoneMasterKey()
	if err != nil {
		return "", err
	}
	key := derivePhoneSubkey(master, phoneHashDomain)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(phoneCryptoInput(zone, phone)))
	return phoneHashKeyVersion + ":" + hex.EncodeToString(mac.Sum(nil)), nil
}

// LoginAccountBlindHash 计算登录标识符（手机号 / 邮箱 / username）的盲索引，
// 格式同 PhoneBlindHash：`<密钥版本>:<HMAC-SHA256 hex>`。
//
// 用途：登录日志需要"按账号精确检索"（尤其失败记录——那时还没解析出 uid，只能靠账号
// 定位"这个手机号被爆破了多少次"），但又不能把手机号/邮箱明文落库。掩码值无法精确
// 检索（多个号共享同一掩码），所以掩码用于人读、盲索引用于精确匹配。
//
// 输入先做 trim + 转小写归一化，保证 "A@b.com" 与 "a@b.com" 命中同一个哈希。
func LoginAccountBlindHash(account string) (string, error) {
	account = strings.ToLower(strings.TrimSpace(account))
	if account == "" {
		return "", nil
	}
	master, err := phoneMasterKey()
	if err != nil {
		return "", err
	}
	key := derivePhoneSubkey(master, loginAccountHashDomain)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(account))
	return phoneHashKeyVersion + ":" + hex.EncodeToString(mac.Sum(nil)), nil
}

// maskLoginAccount 把登录标识符掩码成"可读但不含完整 PII"的形式，供登录日志落库与
// 结构化日志输出。
//
// 统一掩码而不按类型区分是否 PII：username 本身可能就是手机号（手机号注册用户的
// username 是 zone+phone 拼接），靠字段名判断"这个是不是 PII"不可靠，所以一律掩码，
// 精确检索交给 LoginAccountBlindHash。
//
// 全程按 rune 处理，避免多字节输入被切出非法 UTF-8（同 phoneLast4 的教训）。
func maskLoginAccount(account string) string {
	account = strings.TrimSpace(account)
	if account == "" {
		return ""
	}
	// 邮箱：保留首字符 + 完整域名（域名不是个人标识，保留它对排查有价值）
	if at := strings.LastIndex(account, "@"); at > 0 {
		local := []rune(account[:at])
		domain := account[at:]
		if len(local) <= 1 {
			return "*" + domain
		}
		return string(local[0]) + "***" + domain
	}
	r := []rune(account)
	// 纯数字（手机号 / zone+phone 拼接）：保留前 3 后 4
	if len(r) >= 7 && allDigits(r) {
		return string(r[:3]) + "****" + string(r[len(r)-4:])
	}
	if len(r) <= 2 {
		return strings.Repeat("*", len(r))
	}
	return string(r[0]) + "***"
}

func allDigits(r []rune) bool {
	for _, c := range r {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// phoneEncryptor 封装手机号密文的加解密。主密钥在构造期解析一次并缓存派生子密钥，
// 用法与 modules/usersecret 的 encryptor 一致。
type phoneEncryptor struct {
	gcm cipher.AEAD
}

// newPhoneEncryptor 读取主密钥并构造 AES-256-GCM AEAD。主密钥缺失/长度非法时返回
// error；调用方（User.New / Manager.NewManager）据此降级：手机号加密列停写，明文
// phone 列不受影响。
func newPhoneEncryptor() (*phoneEncryptor, error) {
	master, err := phoneMasterKey()
	if err != nil {
		return nil, err
	}
	sub := derivePhoneSubkey(master, phoneEncryptDomain)
	block, err := aes.NewCipher(sub)
	if err != nil {
		return nil, fmt.Errorf("user: phone aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("user: phone cipher.NewGCM: %w", err)
	}
	return &phoneEncryptor{gcm: gcm}, nil
}

// encryptPhone 加密 zone+phone，返回密文（存 phone_encrypted）、盲索引 hex（存
// phone_hash）、后4位明文（存 phone_last4）。phone 为空时返回 error，调用方应
// 先判断 phone != "" 再调用。
func (e *phoneEncryptor) encryptPhone(zone, phone string) (encrypted []byte, hash string, last4 string, err error) {
	if phone == "" {
		return nil, "", "", errors.New("user: encryptPhone requires non-empty phone")
	}
	plaintext := phoneCryptoInput(zone, phone)
	nonce := make([]byte, e.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, "", "", fmt.Errorf("user: phone rand nonce: %w", err)
	}
	sealed := e.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	out := make([]byte, 0, len(phoneCipherVersionPrefix)+len(sealed))
	out = append(out, []byte(phoneCipherVersionPrefix)...)
	out = append(out, sealed...)
	h, err := PhoneBlindHash(zone, phone)
	if err != nil {
		return nil, "", "", err
	}
	return out, h, phoneLast4(phone), nil
}

// decryptPhone 解密 encryptPhone 产生的密文，返回原始的 zone+"|"+phone 拼接串。
// 第一阶暂无生产读路径依赖（明文 phone 列仍是权威读源），保留给测试往返校验和
// 第二阶（停写明文列后按需解密）使用。
func (e *phoneEncryptor) decryptPhone(cipherText []byte) (string, error) {
	if len(cipherText) < len(phoneCipherVersionPrefix) ||
		!strings.HasPrefix(string(cipherText[:len(phoneCipherVersionPrefix)]), phoneCipherVersionPrefix) {
		return "", errors.New("user: unknown phone cipher prefix")
	}
	raw := cipherText[len(phoneCipherVersionPrefix):]
	ns := e.gcm.NonceSize()
	if len(raw) < ns+e.gcm.Overhead() {
		return "", errors.New("user: phone cipher too short")
	}
	plaintext, err := e.gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("user: phone gcm open: %w", err)
	}
	return string(plaintext), nil
}

// ErrPhoneEncryptionUnavailable 有手机号要写、但加密能力不可用（主密钥缺失/非法，
// 或加密本身失败）。写路径必须把它作为 5xx 传播出去，不能降级成"只写明文"。
var ErrPhoneEncryptionUnavailable = errors.New("user: phone encryption unavailable")

// syncPhoneShadow 在写入前把三个手机号影子列同步成与 m.Phone 一致。
//
// 由 DB.Insert / DB.insertTx 统一调用 —— user 表全仓库只有这两个插入口，所以任何
// 建号路径（含未来新增的）都会自动带上影子列，不依赖调用方记得手工调用。
//
// fail-closed：只要 m.Phone 非空而加密能力不可用，就返回 ErrPhoneEncryptionUnavailable
// 让写入整体失败。**不做"只写明文"的降级** —— 那会让 OCTO_PII_ENCRYPTION_SECRET 漏配
// 长期无人发现，明文手机号持续入库，而"手机号已加密存储"这个结论早已对外声明。
// 宁可注册报 5xx（运维立刻看到并补配置），也不要静默积累明文 PII。
//
// 无手机号的用户（username / email / OAuth 注册、机器人账号）不受影响：清空影子列后
// 正常放行，所以缺密钥不会阻断这些建号路径。
//
// 注意 update 路径不在这里覆盖：user.phone 目前唯一的改写点是
// destroyAccountFromState（注销匿名化），它显式清空这三列 —— 影子列若比明文列
// "活得更久"，已注销账号会继续占用原手机号（见该函数注释与
// TestDestroyFreesPhoneForReuse）。新增任何改写 phone 的路径都必须同步这三列。
func (d *DB) syncPhoneShadow(m *Model) error {
	if m == nil {
		return nil
	}
	if m.Phone == "" {
		m.PhoneEncrypted, m.PhoneHash, m.PhoneLast4 = nil, "", ""
		return nil
	}
	if d.phoneEnc == nil {
		return fmt.Errorf("%w: %s must be configured before writing a phone number",
			ErrPhoneEncryptionUnavailable, phoneEncryptionSecretEnv)
	}
	encrypted, hash, last4, err := d.phoneEnc.encryptPhone(m.Zone, m.Phone)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPhoneEncryptionUnavailable, err)
	}
	m.PhoneEncrypted = encrypted
	m.PhoneHash = hash
	m.PhoneLast4 = last4
	return nil
}
