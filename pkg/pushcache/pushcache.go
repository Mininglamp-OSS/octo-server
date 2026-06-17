// Package pushcache centralizes the Redis cache-key scheme for offline push
// notification titles (group name / thread name) and the invalidation helpers
// that keep those caches fresh when the underlying name changes.
//
// The offline-push pipeline (modules/webhook) reads these display names through
// a TTL cache to avoid a database hit on every offline message. Because the
// modules/webhook package imports modules/group, the group/thread packages
// cannot import webhook to bust the cache on rename without creating an import
// cycle. This package is the neutral, lower-level home that both the producer
// (webhook, which builds the keys) and the writers (group/thread, which
// invalidate on rename) can depend on, so the key format lives in exactly one
// place.
package pushcache

import "github.com/Mininglamp-OSS/octo-lib/pkg/redis"

const (
	// groupNamePrefix keys the cached display name of a group, keyed by its
	// group number: groupName:<groupNo>.
	groupNamePrefix = "groupName:"
	// threadNamePrefix keys the cached display name of a thread (sub-area),
	// keyed by its channel ID (<groupNo>____<shortID>): threadName:<channelID>.
	//
	// Only the thread's own name is cached here; the group-name portion of a
	// thread push title is composed at read time from GroupNameKey, so renaming
	// a group only needs to invalidate that single group key for both the group
	// push title and every thread push title under it to refresh.
	threadNamePrefix = "threadName:"
)

// GroupNameKey returns the cache key holding a group's display name.
func GroupNameKey(groupNo string) string {
	return groupNamePrefix + groupNo
}

// ThreadNameKey returns the cache key holding a thread's display name, keyed by
// the thread channel ID (<groupNo>____<shortID>).
func ThreadNameKey(channelID string) string {
	return threadNamePrefix + channelID
}

// InvalidateGroupName drops the cached group display name so the next offline
// push re-reads it from the database. Call it after a group rename. It is
// best-effort: callers should log but not fail their operation on error — the
// cache TTL is the backstop.
func InvalidateGroupName(rc *redis.Conn, groupNo string) error {
	return rc.Del(GroupNameKey(groupNo))
}

// InvalidateThreadName drops the cached thread display name. channelID is the
// thread channel ID (<groupNo>____<shortID>). See InvalidateGroupName for the
// best-effort contract.
func InvalidateThreadName(rc *redis.Conn, channelID string) error {
	return rc.Del(ThreadNameKey(channelID))
}
