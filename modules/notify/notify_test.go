package notify

import (
	"testing"
	"time"
)

// === dedupTargets ===

func TestDedupTargets_Empty(t *testing.T) {
	result := dedupTargets(nil)
	if len(result) != 0 {
		t.Errorf("expected empty, got %v", result)
	}
}

func TestDedupTargets_NoDuplicates(t *testing.T) {
	result := dedupTargets([]string{"a", "b", "c"})
	if len(result) != 3 || result[0] != "a" || result[1] != "b" || result[2] != "c" {
		t.Errorf("expected [a b c], got %v", result)
	}
}

func TestDedupTargets_WithDuplicates(t *testing.T) {
	result := dedupTargets([]string{"a", "b", "a", "c", "b"})
	if len(result) != 3 || result[0] != "a" || result[1] != "b" || result[2] != "c" {
		t.Errorf("expected [a b c], got %v", result)
	}
}

func TestDedupTargets_EmptyStrings(t *testing.T) {
	result := dedupTargets([]string{"", "a", "", "b"})
	if len(result) != 2 || result[0] != "a" || result[1] != "b" {
		t.Errorf("expected [a b], got %v", result)
	}
}

// === NotifyBotUID ===

func TestNotifyBotUID(t *testing.T) {
	uid := NotifyBotUID("sp_abc123")
	expected := "notify_sp_abc123_bot"
	if uid != expected {
		t.Errorf("expected %q, got %q", expected, uid)
	}
}

// === memberCache ===

func TestMemberCache_InvalidateRemovesEntry(t *testing.T) {
	mc := newMemberCache()
	mc.entries["sp1"] = &memberCacheEntry{
		uids:     map[string]bool{"uid_a": true},
		expireAt: time.Now().Add(cacheTTL),
	}

	mc.invalidate("sp1")

	mc.mu.RLock()
	_, exists := mc.entries["sp1"]
	mc.mu.RUnlock()

	if exists {
		t.Error("expected sp1 to be invalidated")
	}
}

func TestMemberCache_InvalidateNonExistent(t *testing.T) {
	mc := newMemberCache()
	// Should not panic
	mc.invalidate("nonexistent")
}

// === model validation ===

func TestNotifyReq_Fields(t *testing.T) {
	req := NotifyReq{
		SpaceID:  "sp_abc",
		Service:  "todo-service",
		Event:    "todo.assigned",
		Targets:  []string{"uid_a", "uid_b"},
		ActorUID: "uid_x",
		Payload:  map[string]interface{}{"type": 1, "content": "test"},
	}
	if req.SpaceID != "sp_abc" {
		t.Error("SpaceID mismatch")
	}
	if len(req.Targets) != 2 {
		t.Error("Targets length mismatch")
	}
}

func TestBatchNotifyResp_HasErrors(t *testing.T) {
	resp := BatchNotifyResp{
		Results: []BatchNotifyResult{
			{NotifyResp: NotifyResp{Delivered: []string{"a"}}, Error: ""},
			{NotifyResp: NotifyResp{Delivered: []string{}}, Error: "space not found"},
		},
		HasErrors: true,
	}
	if !resp.HasErrors {
		t.Error("expected HasErrors=true")
	}
	if resp.Results[0].Error != "" {
		t.Error("first result should have no error")
	}
	if resp.Results[1].Error != "space not found" {
		t.Error("second result should have error")
	}
}
