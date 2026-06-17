package pushcache

import "testing"

func TestGroupNameKey(t *testing.T) {
	if got := GroupNameKey("g123"); got != "groupName:g123" {
		t.Fatalf("GroupNameKey = %q, want %q", got, "groupName:g123")
	}
}

func TestThreadNameKey(t *testing.T) {
	if got := ThreadNameKey("g123____s456"); got != "threadName:g123____s456" {
		t.Fatalf("ThreadNameKey = %q, want %q", got, "threadName:g123____s456")
	}
}
