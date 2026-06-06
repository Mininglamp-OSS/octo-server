package common

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	commonbase "github.com/Mininglamp-OSS/octo-server/modules/base/common"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// secretMask is the placeholder returned in GET responses for
// settingTypeEncrypted columns so cleartext never leaves the server.
const secretMask = "****"

// systemSettingItemReq is one entry in the batch update payload.
type systemSettingItemReq struct {
	Category string `json:"category"`
	Key      string `json:"key"`
	Value    string `json:"value"`
}

// systemSettingUpdateReq is the manager update payload.
type systemSettingUpdateReq struct {
	Items []systemSettingItemReq `json:"items"`
}

// systemSettingItemResp is one entry in the GET response.
//
// Field semantics:
//   - Configured: true iff the DB row exists for this (category, key). The
//     admin UI uses this to distinguish "explicitly set" from "using default".
//   - Value: the DB-stored value, or "" if not configured. For encrypted
//     types this is the secretMask placeholder whenever a non-empty
//     ciphertext is stored; cleartext is never returned.
//   - EffectiveValue: the value currently in effect after applying the
//     DB → yaml → code-default fallback chain. For encrypted types this is
//     secretMask whenever the effective plaintext is non-empty (whether the
//     source is DB or yaml), and "" otherwise. Plaintext is NEVER returned.
type systemSettingItemResp struct {
	Category       string `json:"category"`
	Key            string `json:"key"`
	Configured     bool   `json:"configured"`
	Value          string `json:"value"`
	EffectiveValue string `json:"effective_value"`
	ValueType      string `json:"value_type"`
	Description    string `json:"description"`
}

// systemSettingSchemaResp is the schema metadata surfaced to the admin UI.
type systemSettingSchemaResp struct {
	Category    string `json:"category"`
	Key         string `json:"key"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// systemSettingGetResp wraps both the current values and the schema so the
// admin UI can render a complete form without a second round-trip.
type systemSettingGetResp struct {
	Items  []systemSettingItemResp   `json:"items"`
	Schema []systemSettingSchemaResp `json:"schema"`
}

// listSystemSettings handles GET /v1/manager/common/system_setting.
//
// Read access uses CheckLoginRole — any authenticated admin can view the
// effective config (encrypted columns are masked). Writes require
// SuperAdmin (see updateSystemSettings).
func (m *Manager) listSystemSettings(c *wkhttp.Context) {
	if err := c.CheckLoginRole(); err != nil {
		c.ResponseError(err)
		return
	}
	rows, err := m.systemSettingDB.listAll()
	if err != nil {
		m.Error("查询系统设置失败", zap.Error(err))
		c.ResponseError(errors.New("查询系统设置失败"))
		return
	}

	// Index existing rows so the response can place schema entries with
	// their stored value (or blank when not configured).
	stored := map[string]*systemSettingModel{}
	for _, r := range rows {
		stored[schemaKey(r.Category, r.KeyName)] = r
	}

	items := make([]systemSettingItemResp, 0, len(systemSettingSchema))
	schema := make([]systemSettingSchemaResp, 0, len(systemSettingSchema))
	for _, def := range systemSettingSchema {
		schema = append(schema, systemSettingSchemaResp{
			Category:    def.Category,
			Key:         def.Key,
			Type:        def.Type,
			Description: def.Description,
		})

		item := systemSettingItemResp{
			Category:    def.Category,
			Key:         def.Key,
			ValueType:   def.Type,
			Description: def.Description,
		}
		if row, ok := stored[schemaKey(def.Category, def.Key)]; ok {
			// A row exists. Configured tracks whether the DB explicitly holds a
			// value — an empty Value means "DB row present but cleared back to
			// yaml default" (see TestManagerSystemSetting_BoolEmptyValueResetsToYaml),
			// so we still mark Configured=false in that case.
			item.Configured = row.Value != ""
			if def.Type == settingTypeEncrypted {
				if row.Value != "" {
					item.Value = secretMask
				}
			} else {
				item.Value = row.Value
			}
		}

		// EffectiveValue resolves DB → yaml → code default through the typed
		// getters bound on the schema entry. Encrypted plaintext is replaced
		// with secretMask before serialisation — a yaml SMTP password must
		// never leak through this endpoint.
		if def.Effective != nil {
			effective := def.Effective(m.systemSettings)
			if def.Type == settingTypeEncrypted {
				if effective != "" {
					item.EffectiveValue = secretMask
				}
			} else {
				item.EffectiveValue = effective
			}
		}
		items = append(items, item)
	}

	c.Response(systemSettingGetResp{Items: items, Schema: schema})
}

// updateSystemSettings handles POST /v1/manager/common/system_setting.
//
// Behavior:
//  1. SuperAdmin role required.
//  2. Each item must match a (category, key) in systemSettingSchema —
//     unknown keys are rejected (400) without partial writes.
//  3. Type-specific validation: bool accepts "0"/"1"/"true"/"false";
//     int must parse as base-10 integer; string is unconstrained;
//     encrypted accepts any string (empty means "do not change").
//  4. Encrypted columns: empty value is silently skipped (preserves the
//     existing ciphertext); non-empty values are AES-256-GCM encrypted
//     via encryptKey before storage.
//  5. After all rows are upserted, SystemSettings.Reload is called so
//     this instance serves the new values immediately. Other instances
//     pick it up within reloadTTL.
func (m *Manager) updateSystemSettings(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}

	var req systemSettingUpdateReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if len(req.Items) == 0 {
		c.ResponseError(errors.New("items 不能为空"))
		return
	}

	// Validate everything first so a malformed item never produces a
	// half-applied write. The actual writes happen later in one
	// transaction.
	type prepared struct {
		def   *settingDef
		value string
		skip  bool
	}
	plans := make([]prepared, 0, len(req.Items))
	for _, item := range req.Items {
		def := findSchemaDef(item.Category, item.Key)
		if def == nil {
			c.JSON(http.StatusBadRequest, jsonH{
				"msg": fmt.Sprintf("未知的配置项：%s.%s", item.Category, item.Key),
			})
			return
		}

		p := prepared{def: def, value: item.Value}
		switch def.Type {
		case settingTypeBool:
			normalised, ok := normaliseBool(item.Value)
			if !ok {
				c.JSON(http.StatusBadRequest, jsonH{
					"msg": fmt.Sprintf("%s.%s 仅接受 0/1/true/false", item.Category, item.Key),
				})
				return
			}
			p.value = normalised
		case settingTypeInt:
			// Empty value means "reset to default" (the getter treats an empty
			// snapshot entry as not-configured), so only validate non-empty
			// payloads. A Positive key (rate-limit / quota knob) must be a
			// strictly-positive int and skips the shared day-window bound;
			// otherwise bounds-check against [settingIntMin, settingIntMax] —
			// previously this path only ran strconv.Atoi with no range guard,
			// letting absurd windows (or negatives) reach the DB (issue #289).
			if item.Value != "" {
				n, err := strconv.Atoi(item.Value)
				if err != nil {
					c.JSON(http.StatusBadRequest, jsonH{
						"msg": fmt.Sprintf("%s.%s 必须是整数", item.Category, item.Key),
					})
					return
				}
				if def.Positive {
					if n <= 0 {
						c.JSON(http.StatusBadRequest, jsonH{
							"msg": fmt.Sprintf("%s.%s 必须是正整数", item.Category, item.Key),
						})
						return
					}
				} else if n < settingIntMin || n > settingIntMax {
					c.JSON(http.StatusBadRequest, jsonH{
						"msg": fmt.Sprintf("%s.%s 必须在 [%d, %d] 范围内", item.Category, item.Key, settingIntMin, settingIntMax),
					})
					return
				}
			}
		case settingTypeFloat:
			// Reject non-finite (NaN / ±Inf — all of which strconv.ParseFloat
			// happily accepts) so they can never reach a rate limiter: a NaN rps
			// errors the Redis Lua script (fail-open) and ±Inf/≤0 short-circuit
			// the bucket to "always allowed". A Positive key additionally rejects
			// ≤0. Read side mirrors this (clamp → default) for direct DB edits.
			if item.Value != "" {
				f, err := strconv.ParseFloat(item.Value, 64)
				if err != nil {
					c.JSON(http.StatusBadRequest, jsonH{
						"msg": fmt.Sprintf("%s.%s 必须是数字", item.Category, item.Key),
					})
					return
				}
				if math.IsNaN(f) || math.IsInf(f, 0) {
					c.JSON(http.StatusBadRequest, jsonH{
						"msg": fmt.Sprintf("%s.%s 必须是有限数字", item.Category, item.Key),
					})
					return
				}
				if def.Positive && f <= 0 {
					c.JSON(http.StatusBadRequest, jsonH{
						"msg": fmt.Sprintf("%s.%s 必须是正数", item.Category, item.Key),
					})
					return
				}
			}
		case settingTypeEncrypted:
			if item.Value == "" || item.Value == secretMask {
				// Empty payload or the GET mask sentinel preserves the existing
				// ciphertext — do not queue an upsert that would blank it out
				// or accidentally store "****" as the real password.
				p.skip = true
				break
			}
			enc, err := encryptKey(item.Value)
			if err != nil {
				// The underlying error (e.g. "OCTO_MASTER_KEY not
				// configured") describes server-internal state — do not
				// leak it over HTTP, log it for ops to find.
				m.Error("加密配置失败",
					zap.String("category", item.Category),
					zap.String("key", item.Key),
					zap.Error(err))
				c.ResponseError(errors.New("加密配置失败，请检查服务端密钥配置"))
				return
			}
			p.value = enc
		case settingTypeString:
			// Anything goes.
		}
		plans = append(plans, p)
	}

	// Atomic batch: open one transaction, queue every upsert, commit only
	// if all rows succeed. A mid-batch DB failure rolls back everything
	// rather than leaving callers to debug partial state.
	tx, err := m.systemSettingDB.beginTx()
	if err != nil {
		m.Error("开启事务失败", zap.Error(err))
		c.ResponseError(errors.New("写入系统设置失败"))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for _, p := range plans {
		if p.skip {
			continue
		}
		if err := m.systemSettingDB.upsertWithTx(
			tx, p.def.Category, p.def.Key, p.value, p.def.Type, p.def.Description,
		); err != nil {
			m.Error("写入系统设置失败", zap.Error(err))
			c.ResponseError(errors.New("写入系统设置失败"))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		m.Error("提交事务失败", zap.Error(err))
		c.ResponseError(errors.New("写入系统设置失败"))
		return
	}
	committed = true

	if err := m.systemSettings.Reload(); err != nil {
		// Reload is best-effort — the row is already persisted, so other
		// instances and the next auto-reload tick will pick it up.
		m.Warn("Reload SystemSettings 失败，等待自动刷新", zap.Error(err))
	}

	// 写入若涉及 login.local_off,直接用刚刚校验过的 plan.value 触发 safety
	// override 日志。**不** 读 snapshot —— 那会被 Reload 失败路径吞掉信号
	// (PR #104 P2 from yujiawei)。故意不阻塞写入:允许 admin 在尚未配置 SSO
	// 时先把开关写到 DB(例如先准备运维下一步切流的开关位),LocalLoginOff()
	// 的安全回退会保证此时仍可本地登录;日志是唯一信号,告诉运维"开关已
	// 落库但当前未生效,先去补 SSO 配置"。
	for _, p := range plans {
		if p.def.Category == "login" && p.def.Key == "local_off" {
			// p.value 已被 normaliseBool 规范成 "0" / "1" / ""(空=回退 yaml,
			// 即 false)。任何非 "1" 都不视为开启。
			m.systemSettings.LogLocalLoginOffSafetyOverrideIfActive(p.value == "1")
			break
		}
	}

	c.ResponseOK()
}

// testSystemSettingEmail handles POST /v1/manager/common/system_setting/test_email.
//
// Sends a no-op test message to the requested address using the currently
// effective SMTP config (DB values, falling back to yaml). Lets admins
// validate SMTP credentials without registering a real user.
func (m *Manager) testSystemSettingEmail(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}

	var req struct {
		To string `json:"to"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if req.To == "" {
		c.ResponseError(errors.New("收件人 to 不能为空"))
		return
	}

	emailSvc := commonbase.NewEmailService(m.ctx, m.systemSettings)
	// Pre-send log:遇到「投出去但收件人没收到」时,这条记录至少证明 endpoint
	// 走到了发送阶段,排查时可以直接对比 SMTP 服务器 sent log;同时把当前
	// effective 的发件人 / 服务器记下来,方便确认 DB override 与 yaml fallback
	// 的最终生效值,避免再去 GET /system_setting 二次核对。
	m.Info("SMTP 测试邮件已尝试投递",
		zap.String("to", req.To),
		zap.String("from", m.systemSettings.SupportEmail()),
		zap.String("smtp", m.systemSettings.SupportEmailSmtp()))

	if err := emailSvc.SendTransactionalHTML(
		c.Request.Context(),
		req.To,
		smtpTestEmailSubject,
		smtpTestEmailHTML,
		smtpTestEmailPlain,
	); err != nil {
		// 错误内文(SMTP 服务器返回码、host:port、TLS 握手细节)留在服务端
		// 日志,不回客户端;avoid leaking infra detail to the admin UI 即使
		// 调用方是 SuperAdmin,降低 HAR 抓包外流时的信息量。
		m.Error("SMTP 测试邮件投递失败",
			zap.String("to", req.To),
			zap.String("from", m.systemSettings.SupportEmail()),
			zap.String("smtp", m.systemSettings.SupportEmailSmtp()),
			zap.Error(err))
		c.ResponseError(errors.New("发送失败，请查看服务端日志"))
		return
	}
	m.Info("SMTP 测试邮件已交付 SMTP 服务器", zap.String("to", req.To))
	c.ResponseOK()
}

// SMTP 测试邮件的标题 / HTML / plaintext 模板。
//
// 设计取舍:把 v5 富版本内联在这里 —— 而不是抽到 modules/base/common —— 因为
// 内容("自检结果 / 请勿回复")是这条 admin endpoint 自己的语义,base 层只
// 负责 SMTP 投递的基础设施。SendTransactionalHTML 会负责包 multipart +
// 加事务邮件 header,本处不再操心这些。
//
// HTML 用 inline CSS + <table> 布局,确保在 Outlook / 网易/腾讯邮箱客户端
// 等对 <style> 块支持差的客户端上也能正常渲染;字体栈带 PingFang SC /
// Microsoft YaHei,保证中英文同字号视觉一致。
//
// plaintext 是 RFC 2046 要求的 alternative,反垃圾过滤器把"是否提供
// plain"作为 transactional vs spam 的关键打分项;不是给"看不懂 HTML 的
// 收件人"准备的,是给打分器看的。
const smtpTestEmailSubject = "[Octo] SMTP 配置自检 · SMTP Configuration Test"

const smtpTestEmailPlain = `Octo SMTP 配置自检 · SMTP Configuration Test
============================================

[ 自动邮件 · 请勿回复 / Automated · Do Not Reply ]

✓ SMTP 主机连接正常        Host connection OK
✓ 发信凭据有效              Credentials valid
✓ 反垃圾策略通过            Anti-spam check passed

如果您收到了这封邮件，说明 Octo 系统的 SMTP 配置一切正常，无需任何操作。
If you received this, your SMTP setup is working. No action needed.

——
Octo System Notification · Octo 系统通知
`

const smtpTestEmailHTML = `<!doctype html>
<html lang="zh-CN">
<body style="margin:0; padding:0; background:#f3f4f6; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'Microsoft YaHei', Roboto, sans-serif; color:#1f2937; line-height:1.6;">
  <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%" style="background:#f3f4f6; padding:32px 16px;">
    <tr>
      <td align="center">
        <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="560" style="max-width:560px; background:#ffffff; border-radius:12px; box-shadow:0 1px 3px rgba(0,0,0,0.06); overflow:hidden;">
          <tr>
            <td style="padding:32px 32px 8px;">
              <h1 style="margin:0; font-size:22px; font-weight:600; color:#111827; letter-spacing:-0.01em;">Octo SMTP 配置自检</h1>
              <div style="margin-top:4px; font-size:13px; color:#6b7280;">SMTP Configuration Test</div>
            </td>
          </tr>
          <tr>
            <td style="padding:16px 32px 0;">
              <div style="background:#fef3c7; border-left:3px solid #f59e0b; padding:10px 14px; border-radius:4px; font-size:13px; color:#78350f;">
                <strong>自动邮件 · 请勿回复</strong> &nbsp;<span style="color:#92400e;">Automated · Do Not Reply</span>
              </div>
            </td>
          </tr>
          <tr>
            <td style="padding:20px 32px 0;">
              <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%" style="border:1px solid #e5e7eb; border-radius:8px;">
                <tr>
                  <td style="padding:12px 16px; border-bottom:1px solid #f3f4f6;">
                    <span style="display:inline-block; width:20px; color:#10b981; font-weight:700;">&#10003;</span>
                    <span style="font-size:15px; color:#111827;">SMTP 主机连接正常</span>
                    <span style="float:right; font-size:13px; color:#6b7280;">Host connection OK</span>
                  </td>
                </tr>
                <tr>
                  <td style="padding:12px 16px; border-bottom:1px solid #f3f4f6;">
                    <span style="display:inline-block; width:20px; color:#10b981; font-weight:700;">&#10003;</span>
                    <span style="font-size:15px; color:#111827;">发信凭据有效</span>
                    <span style="float:right; font-size:13px; color:#6b7280;">Credentials valid</span>
                  </td>
                </tr>
                <tr>
                  <td style="padding:12px 16px;">
                    <span style="display:inline-block; width:20px; color:#10b981; font-weight:700;">&#10003;</span>
                    <span style="font-size:15px; color:#111827;">反垃圾策略通过</span>
                    <span style="float:right; font-size:13px; color:#6b7280;">Anti-spam check passed</span>
                  </td>
                </tr>
              </table>
            </td>
          </tr>
          <tr>
            <td style="padding:20px 32px 24px;">
              <p style="margin:0 0 8px; font-size:14px; color:#374151;">如果您收到了这封邮件，说明 Octo 系统的 SMTP 配置一切正常，无需任何操作。</p>
              <p style="margin:0; font-size:13px; color:#6b7280;">If you received this, your SMTP setup is working. No action needed.</p>
            </td>
          </tr>
          <tr>
            <td style="padding:16px 32px 24px; border-top:1px solid #f3f4f6;">
              <div style="font-size:12px; color:#9ca3af;">
                Octo System Notification &middot; Octo 系统通知
              </div>
            </td>
          </tr>
        </table>
      </td>
    </tr>
  </table>
</body>
</html>
`

// jsonH is a tiny alias for inline JSON payloads. We define a local alias
// instead of importing gin.H to keep the surface visible at the call site.
type jsonH = map[string]interface{}

// normaliseBool canonicalises any accepted bool spelling to "0" / "1" so
// the raw DB rows are consistent regardless of admin UI capitalisation.
// An empty string is also valid and means "reset to yaml default"; the
// getter side treats empty as "not configured". Returns (value, true) on
// success or ("", false) for an unrecognised spelling.
func normaliseBool(v string) (string, bool) {
	switch v {
	case "":
		return "", true
	case "0", "false", "FALSE":
		return "0", true
	case "1", "true", "TRUE":
		return "1", true
	}
	return "", false
}
