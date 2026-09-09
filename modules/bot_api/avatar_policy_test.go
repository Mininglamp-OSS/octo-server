package bot_api

import "testing"

func TestAvatarRouteFamiliesDefaultDeny(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{"POST", "/v1/bot/createGroup"}, {"PUT", "/v1/bot/groups/:group_no/info"},
		{"POST", "/v1/bot/groups/:group_no/members/add"}, {"POST", "/v1/bot/groups/:group_no/members/remove"},
		{"POST", "/v1/bot/groups/:group_no/incoming-webhooks"},
		{"POST", "/v1/bot/groups/:group_no/threads/:short_id/incoming-webhooks"},
		{"POST", "/v1/bot/groups/:group_no/threads"}, {"DELETE", "/v1/bot/groups/:group_no/threads/:short_id"},
		{"PUT", "/v1/bot/groups/:group_no/md"}, {"PUT", "/v1/bot/groups/:group_no/threads/:short_id/md"},
		{"POST", "/v1/bot/users/batch"}, {"GET", "/v1/bot/space/members"},
		{"GET", "/v1/bot/messages/_search"}, {"POST", "/v1/bot/voice/transcribe"},
		{"GET", "/v1/bot/obo-grant"}, {"GET", "/v1/bot/space/principals/:uid"},
		{"POST", "/v1/bot/future-endpoint"}, {"DELETE", "/v1/bot/groups/:group_no"},
	} {
		if avatarRouteAllowed(route.method, route.path) {
			t.Errorf("must deny %s %s", route.method, route.path)
		}
	}
	for _, route := range []string{"/v1/bot/sendMessage", "/v1/bot/messages/sync", "/v1/bot/readReceipt", "/v1/bot/typing", "/v1/bot/events"} {
		if !avatarRouteAllowed("POST", route) {
			t.Errorf("must allow %s", route)
		}
	}
	if !avatarRouteAllowed("GET", "/v1/bot/groups/:group_no/threads/:short_id") {
		t.Fatal("thread reading must be allowed")
	}
}
