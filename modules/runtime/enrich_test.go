package runtime

import (
	"encoding/json"
	"testing"
)

func TestBuildRouteInfos(t *testing.T) {
	bots := map[string]botInfo{
		"27gl5wrM9gA_bot": {UID: "27gl5wrM9gA_bot", Name: "Bot A"},
	}
	cases := []struct {
		name   string
		route  string
		online bool
		want   routeInfo
	}{
		{
			name:   "dmwork bot hit + online",
			route:  "octo/27gl5wrM9gA_bot",
			online: true,
			want: routeInfo{
				Raw: "octo/27gl5wrM9gA_bot", Channel: "octo",
				UID: "27gl5wrM9gA_bot", Name: "Bot A", IsBot: true, Online: true,
			},
		},
		{
			name:   "dmwork bot hit + offline",
			route:  "octo/27gl5wrM9gA_bot",
			online: false,
			want: routeInfo{
				Raw: "octo/27gl5wrM9gA_bot", Channel: "octo",
				UID: "27gl5wrM9gA_bot", Name: "Bot A", IsBot: true, Online: false,
			},
		},
		{
			name:   "dmwork uid not in bots → uid only, is_bot false",
			route:  "octo/strangeruid_bot",
			online: true,
			want: routeInfo{
				Raw: "octo/strangeruid_bot", Channel: "octo",
				UID: "strangeruid_bot", IsBot: false, Online: false,
			},
		},
		{
			name:   "dmwork with empty uid",
			route:  "octo/",
			online: true,
			want: routeInfo{
				Raw: "octo/", Channel: "octo", UID: "", IsBot: false, Online: false,
			},
		},
		{
			name:   "wecom channel → account_id only",
			route:  "wecom/webhook_abc",
			online: true,
			want: routeInfo{
				Raw: "wecom/webhook_abc", Channel: "wecom",
				AccountID: "webhook_abc", IsBot: false, Online: false,
			},
		},
		{
			name:   "no slash → channel empty, account_id=raw",
			route:  "justraw",
			online: true,
			want: routeInfo{
				Raw: "justraw", AccountID: "justraw", IsBot: false, Online: false,
			},
		},
		{
			name:   "leading slash → channel empty, account_id=raw",
			route:  "/noChannel",
			online: true,
			want: routeInfo{
				Raw: "/noChannel", AccountID: "/noChannel", IsBot: false, Online: false,
			},
		},
		{
			name:   "multi-slash dmwork uid preserved",
			route:  "octo/odd/multi/part",
			online: true,
			want: routeInfo{
				Raw: "octo/odd/multi/part", Channel: "octo",
				UID: "odd/multi/part", IsBot: false, Online: false,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRouteInfo(c.route, bots, c.online)
			if got != c.want {
				t.Errorf("got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestBuildRouteInfos_PreservesOrder(t *testing.T) {
	routes := []string{"octo/a_bot", "wecom/b", "octo/c_bot"}
	bots := map[string]botInfo{"a_bot": {UID: "a_bot", Name: "A"}}
	got := buildRouteInfos(routes, bots, true)
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3", len(got))
	}
	if got[0].UID != "a_bot" || !got[0].IsBot {
		t.Errorf("[0] wrong: %+v", got[0])
	}
	if got[1].Channel != "wecom" || got[1].AccountID != "b" {
		t.Errorf("[1] wrong: %+v", got[1])
	}
	if got[2].UID != "c_bot" || got[2].IsBot {
		t.Errorf("[2] should be uid-only (not in bots): %+v", got[2])
	}
}

func TestCollectDmworkUIDs(t *testing.T) {
	seen := map[string]struct{}{}
	collectDmworkUIDs([]string{
		"octo/a_bot",
		"wecom/skip_me",
		"octo/b_bot",
		"octo/",       // 空 uid 忽略
		"octo/a_bot",  // 重复，set 去重
		"no_slash",      // 无 / 忽略
		"/empty_channel",
	}, seen)

	want := map[string]bool{"a_bot": true, "b_bot": true}
	if len(seen) != len(want) {
		t.Errorf("seen=%v, want %v", seen, want)
	}
	for k := range want {
		if _, ok := seen[k]; !ok {
			t.Errorf("missing %q", k)
		}
	}
}

// Regression: openclaw runtime 只有外部 channel 绑定（无 dmwork/<uid>）时
// 仍要生成 route_infos 字段，保证 API 响应结构一致。
func TestInjectRouteInfos_ExternalOnly(t *testing.T) {
	meta := `{"cli_version":"1.0","agents":[
		{"id":"main","bindings":2,"routes":["wecom/webhook_a","feishu/chan_b"]}
	]}`
	enriched, ok := injectRouteInfos(meta, map[string]botInfo{}, true)
	if !ok {
		t.Fatal("injectRouteInfos should succeed for valid metadata")
	}

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(enriched), &got); err != nil {
		t.Fatalf("unmarshal enriched: %v", err)
	}
	agents, _ := got["agents"].([]interface{})
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	agentMap := agents[0].(map[string]interface{})

	// routes 保留不变
	origRoutes, _ := agentMap["routes"].([]interface{})
	if len(origRoutes) != 2 {
		t.Errorf("routes should be preserved intact, got %d items", len(origRoutes))
	}

	// route_infos 应该生成，且所有条目都是非 dmwork 分支（is_bot=false，有 account_id）
	infos, _ := agentMap["route_infos"].([]interface{})
	if len(infos) != 2 {
		t.Fatalf("expected 2 route_infos, got %d", len(infos))
	}
	for i, ri := range infos {
		m := ri.(map[string]interface{})
		if m["is_bot"] != false {
			t.Errorf("route_infos[%d].is_bot should be false, got %v", i, m["is_bot"])
		}
		if m["account_id"] == nil || m["account_id"] == "" {
			t.Errorf("route_infos[%d] should have account_id, got %+v", i, m)
		}
	}
}

// route_infos 总是覆盖每个 agent，即使没有 routes 字段，也保留 agent 其他原字段。
func TestInjectRouteInfos_PreservesAgentFields(t *testing.T) {
	meta := `{"agents":[
		{"id":"main","name":"Main","bindings":1,"is_default":true,"custom_field":"keep_me",
		 "routes":["octo/x_bot"]}
	]}`
	enriched, ok := injectRouteInfos(meta, map[string]botInfo{"x_bot": {UID: "x_bot", Name: "X"}}, true)
	if !ok {
		t.Fatal("inject failed")
	}
	var got map[string]interface{}
	json.Unmarshal([]byte(enriched), &got)
	agent := got["agents"].([]interface{})[0].(map[string]interface{})

	// 原字段都在
	if agent["id"] != "main" || agent["name"] != "Main" ||
		agent["is_default"] != true || agent["custom_field"] != "keep_me" {
		t.Errorf("original agent fields lost: %+v", agent)
	}
	// route_infos 注入
	infos := agent["route_infos"].([]interface{})
	if len(infos) != 1 {
		t.Fatalf("expected 1 route_info, got %d", len(infos))
	}
	info := infos[0].(map[string]interface{})
	if info["is_bot"] != true || info["name"] != "X" {
		t.Errorf("enrich mismatch: %+v", info)
	}
}

func TestInjectRouteInfos_MalformedReturnsFalse(t *testing.T) {
	_, ok := injectRouteInfos(`not json`, map[string]botInfo{}, true)
	if ok {
		t.Error("expected false for malformed metadata")
	}
}
