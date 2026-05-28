package common

// Value types accepted by system_setting.value_type.
const (
	settingTypeString    = "string"
	settingTypeBool      = "bool"
	settingTypeInt       = "int"
	settingTypeEncrypted = "encrypted"
)

// settingDef is the canonical definition of a system_setting key.
// The schema slice below is the single source of truth: admin UI reads it to
// render the form, the helper consults it for type info, and the manager
// API rejects writes whose (category, key) is not present here.
type settingDef struct {
	Category    string
	Key         string
	Type        string // settingTypeString | settingTypeBool | settingTypeInt | settingTypeEncrypted
	Description string
	// Effective returns the value that is currently in effect for this
	// setting, applying the DB → yaml → code-default fallback chain. The
	// listSystemSettings handler uses this to populate `effective_value`
	// in the GET response so the admin UI can render the actual running
	// value even when the DB row is absent.
	//
	// For settingTypeEncrypted, the returned string is plaintext — the
	// API layer is responsible for masking before serialisation; never
	// surface this value directly.
	Effective func(*SystemSettings) string
}

// systemSettingSchema enumerates every admin-tunable setting backed by the
// system_setting table. To add a new setting, append a row here and use the
// generic SystemSettings.getBool / getString / getInt / getEncrypted getter
// — no schema migration is required.
var systemSettingSchema = []settingDef{
	// Registration toggles — formerly yaml-only (Register.* in config.go).
	{Category: "register", Key: "off", Type: settingTypeBool, Description: "是否关闭注册",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.RegisterOff()) }},
	{Category: "register", Key: "only_china", Type: settingTypeBool, Description: "仅中国手机号可以注册",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.RegisterOnlyChina()) }},
	{Category: "register", Key: "username_on", Type: settingTypeBool, Description: "是否开启用户名注册",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.RegisterUsernameOn()) }},
	{Category: "register", Key: "email_on", Type: settingTypeBool, Description: "是否开启邮箱注册/登录",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.RegisterEmailOn()) }},

	// Local-account login master toggle — when on, hides local login UI and
	// rejects /v1/user/login, /v1/user/usernamelogin, /v1/user/emaillogin so
	// SSO-only deployments can route all users through OIDC/GitHub/Gitee.
	{Category: "login", Key: "local_off", Type: settingTypeBool, Description: "是否关闭本地账号登录入口",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.LocalLoginOff()) }},

	// Space user-facing creation toggle — admin 关闭后客户端隐藏创建入口,
	// 后端 POST /v1/space/create 直接 403。env DM_SPACE_DISABLE_USER_CREATE
	// 仍作 fallback,DB 行为单一真源。
	{Category: "space", Key: "disable_user_create", Type: settingTypeBool, Description: "是否关闭普通用户创建空间入口",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.SpaceDisableUserCreate()) }},

	// Email server config — formerly yaml-only (Support.* in config.go).
	{Category: "support", Key: "email", Type: settingTypeString, Description: "技术支持邮箱（发件人）",
		Effective: func(s *SystemSettings) string { return s.SupportEmail() }},
	{Category: "support", Key: "email_smtp", Type: settingTypeString, Description: "SMTP 服务器 host:port",
		Effective: func(s *SystemSettings) string { return s.SupportEmailSmtp() }},
	{Category: "support", Key: "email_pwd", Type: settingTypeEncrypted, Description: "SMTP 密码（加密存储）",
		Effective: func(s *SystemSettings) string { return s.SupportEmailPwd() }},
}

// boolToCanonical normalises a bool to the same "0"/"1" representation that
// normaliseBool writes to the DB, so GET effective_value and POST request
// payloads use a single spelling end-to-end.
func boolToCanonical(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// schemaKey returns the canonical "category.key" string used as map key in
// the helper snapshot.
func schemaKey(category, key string) string {
	return category + "." + key
}

// findSchemaDef returns the schema entry for (category, key), or nil if not
// registered. Manager API write path uses this to reject unknown keys.
func findSchemaDef(category, key string) *settingDef {
	for i := range systemSettingSchema {
		d := &systemSettingSchema[i]
		if d.Category == category && d.Key == key {
			return d
		}
	}
	return nil
}
