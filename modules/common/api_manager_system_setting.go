package common

import (
	"errors"
	"fmt"
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
			if item.Value != "" {
				if _, err := strconv.Atoi(item.Value); err != nil {
					c.JSON(http.StatusBadRequest, jsonH{
						"msg": fmt.Sprintf("%s.%s 必须是整数", item.Category, item.Key),
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
	if err := emailSvc.SendHTMLEmail(
		c.Request.Context(),
		req.To,
		"Octo SMTP 测试邮件",
		`<p>这是一封来自 Octo 管理后台的测试邮件。如果你收到了它，说明 SMTP 配置正常。</p>`,
	); err != nil {
		c.ResponseError(fmt.Errorf("发送失败: %w", err))
		return
	}
	c.ResponseOK()
}

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
