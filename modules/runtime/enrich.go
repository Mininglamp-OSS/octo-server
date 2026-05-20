package runtime

import (
	"encoding/json"
	"strings"

	"go.uber.org/zap"
)

// botInfo 是 enrich 查询返回的合法 bot 最小信息。
type botInfo struct {
	UID  string
	Name string
}

// routeInfo 是注入到 list 响应 metadata.agents[].route_infos 的单元。
// daemon 原始上报的字符串 routes 字段保持不变，这里是平行字段。
type routeInfo struct {
	Raw       string `json:"raw"`
	Channel   string `json:"channel"`
	UID       string `json:"uid,omitempty"`
	Name      string `json:"name,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	IsBot     bool   `json:"is_bot"`
	// Online 不带 omitempty：offline 要明确序列化为 false，便于 E2E / DB 观测。
	Online bool `json:"online"`
}

// buildRouteInfos 把 daemon 上报的 "channel/accountId" 字符串数组转成 routeInfo。
// 纯函数，不碰 DB；bots 参数由调用方批量查询得到（仅合法 dmwork bot）。
//
// 异常格式处理（除命中 bot 的 dmwork 外一律 is_bot=false）：
//   - "octo/<uid>" + bots[uid] 命中   → is_bot=true, name 填, online=<online 参数>
//   - "octo/<uid>" + bots 未命中      → 仅 uid，不给 name
//   - "octo/" 空 uid                   → uid=""
//   - "wecom/xxx" / "feishu/xxx" 等      → channel + account_id，不查表
//   - 无 `/` 或空 channel                → channel="", account_id=raw
//   - 多 slash（如 "octo/a/b"）        → uid="a/b" 整段保留
func buildRouteInfos(routes []string, bots map[string]botInfo, online bool) []routeInfo {
	out := make([]routeInfo, 0, len(routes))
	for _, raw := range routes {
		out = append(out, parseRouteInfo(raw, bots, online))
	}
	return out
}

func parseRouteInfo(raw string, bots map[string]botInfo, online bool) routeInfo {
	info := routeInfo{Raw: raw}

	slashIdx := strings.Index(raw, "/")
	if slashIdx <= 0 {
		// 无 `/` 或以 `/` 开头（空 channel）：不拆，整段当 account_id 兜底
		info.AccountID = raw
		return info
	}

	info.Channel = raw[:slashIdx]
	accountID := raw[slashIdx+1:]

	if info.Channel == "octo" {
		info.UID = accountID // 空 uid 也保留为 ""
		if accountID != "" {
			if b, ok := bots[accountID]; ok {
				info.Name = b.Name
				info.IsBot = true
				info.Online = online
			}
		}
		return info
	}

	// 非 dmwork channel：只保留 channel + account_id，不查表
	info.AccountID = accountID
	return info
}

// collectDmworkUIDs 从一组 routes 中收集 channel=="octo" 的非空 uid。
// 用于 enrich 阶段的第一遍扫描，后续送进 queryBotInfoByUIDs 做批量查询。
func collectDmworkUIDs(routes []string, seen map[string]struct{}) {
	for _, raw := range routes {
		slashIdx := strings.Index(raw, "/")
		if slashIdx <= 0 {
			continue
		}
		if raw[:slashIdx] != "octo" {
			continue
		}
		uid := raw[slashIdx+1:]
		if uid == "" {
			continue
		}
		seen[uid] = struct{}{}
	}
}

// enrichRuntimeRouteInfos 遍历 list 里的 openclaw runtime，把 metadata.agents[].routes
// 注入 route_infos 字段（平行字段，不改 routes 本身）。失败 fall through 保持原 metadata。
//
// 设计决策：
//   - 只处理 provider == "openclaw"：其他 provider 如未来 metadata 偶然带 agents 字段，不误解析
//   - slice 按下标写回（list 是值切片，for _, r := range 改的是副本会丢）
//   - 用 map[string]interface{} 解析 + 往 agent map 新增 route_infos key，不重建 agent 结构，
//     避免丢失 daemon 带来的未来新字段
//   - routes 字段 JSON 解出来是 []interface{}，元素类型断言 .(string) 防御异常数据
func (rt *Runtime) enrichRuntimeRouteInfos(list []runtimeResp, spaceID string) {
	// 第一遍：收集所有 openclaw runtime 里 dmwork channel 的 uid（去重）。
	// 同时记录是否存在任何 openclaw runtime —— 即使 uid 全为空（纯 external
	// channel 如 wecom），后续仍要为这些 runtime 生成空 route_infos，
	// 保证 API 响应结构一致。
	uidSet := map[string]struct{}{}
	hasOpenclaw := false
	for i := range list {
		if list[i].Provider != "openclaw" || list[i].Metadata == "" {
			continue
		}
		hasOpenclaw = true
		routes := extractAllRoutes(list[i].Metadata)
		collectDmworkUIDs(routes, uidSet)
	}
	if !hasOpenclaw {
		return
	}

	// 批量查合法 bot；uidSet 为空时跳过查询直接用空 map，
	// 后续仍会为每个 openclaw runtime 生成 route_infos（结构一致）。
	bots := map[string]botInfo{}
	if len(uidSet) > 0 {
		uids := make([]string, 0, len(uidSet))
		for uid := range uidSet {
			uids = append(uids, uid)
		}
		queried, err := rt.db.queryBotInfoByUIDs(spaceID, uids)
		if err != nil {
			rt.Warn("enrich route_infos: query bot info failed", zap.Error(err))
		} else {
			bots = queried
		}
	}

	// 第二遍：为每个 openclaw runtime 写回 metadata
	for i := range list {
		if list[i].Provider != "openclaw" || list[i].Metadata == "" {
			continue
		}
		enriched, ok := injectRouteInfos(list[i].Metadata, bots, list[i].Status == "online")
		if !ok {
			continue // 解析失败，保持原样
		}
		list[i].Metadata = enriched
	}
}

// extractAllRoutes 扫描 metadata JSON 里 agents[].routes 字符串元素。
// 非 string 元素（防御异常数据）跳过。
func extractAllRoutes(metaJSON string) []string {
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return nil
	}
	agents, _ := meta["agents"].([]interface{})
	var out []string
	for _, a := range agents {
		agentMap, _ := a.(map[string]interface{})
		if agentMap == nil {
			continue
		}
		rawRoutes, _ := agentMap["routes"].([]interface{})
		for _, r := range rawRoutes {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// injectRouteInfos 解析 metadata、给每个 agent 增加 route_infos 字段，重新 marshal。
// 返回 (new metadata string, true) 或 ("", false) 表示失败。
func injectRouteInfos(metaJSON string, bots map[string]botInfo, online bool) (string, bool) {
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return "", false
	}
	agents, _ := meta["agents"].([]interface{})
	for _, a := range agents {
		agentMap, _ := a.(map[string]interface{})
		if agentMap == nil {
			continue
		}
		rawRoutes, _ := agentMap["routes"].([]interface{})
		routeStrs := make([]string, 0, len(rawRoutes))
		for _, r := range rawRoutes {
			if s, ok := r.(string); ok {
				routeStrs = append(routeStrs, s)
			}
		}
		agentMap["route_infos"] = buildRouteInfos(routeStrs, bots, online)
	}
	out, err := json.Marshal(meta)
	if err != nil {
		return "", false
	}
	return string(out), true
}
