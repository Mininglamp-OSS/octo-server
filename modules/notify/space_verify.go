package notify

import (
	"sync"
	"time"

	"github.com/gocraft/dbr/v2"
	"golang.org/x/sync/singleflight"
)

const (
	cacheTTL     = 60 * time.Second
	maxCacheSize = 10000 // 防止无限增长
)

// memberCache 实例级 Space 成员缓存。
type memberCache struct {
	mu      sync.RWMutex
	entries map[string]*memberCacheEntry
	sf      singleflight.Group
}

type memberCacheEntry struct {
	uids     map[string]bool
	expireAt time.Time
}

func newMemberCache() *memberCache {
	return &memberCache{
		entries: make(map[string]*memberCacheEntry),
	}
}

// verify 校验 targets 中哪些是 spaceID 的活跃成员。
func (mc *memberCache) verify(db *dbr.Session, spaceID string, targets []string) (members []string, filtered map[string]string, err error) {
	filtered = make(map[string]string)
	if len(targets) == 0 {
		return
	}

	validSet, err := mc.getValidMembers(db, spaceID, targets)
	if err != nil {
		return nil, nil, err
	}

	for _, uid := range targets {
		if validSet[uid] {
			members = append(members, uid)
		} else {
			filtered[uid] = "not_space_member"
		}
	}
	return
}

// getValidMembers 从缓存或 DB 获取有效成员集合。
// B3 修复：cache miss 时先 refresh（单次全量查询），再从缓存过滤。单次 DB 往返。
func (mc *memberCache) getValidMembers(db *dbr.Session, spaceID string, targets []string) (map[string]bool, error) {
	mc.mu.RLock()
	entry, ok := mc.entries[spaceID]
	mc.mu.RUnlock()

	if !ok || time.Now().After(entry.expireAt) {
		if _, err, _ := mc.sf.Do(spaceID, func() (interface{}, error) {
			return nil, mc.refresh(db, spaceID)
		}); err != nil {
			return nil, err
		}
		mc.mu.RLock()
		entry, ok = mc.entries[spaceID]
		mc.mu.RUnlock()
		if !ok {
			return nil, nil
		}
	}

	result := make(map[string]bool, len(targets))
	for _, uid := range targets {
		if entry.uids[uid] {
			result[uid] = true
		}
	}
	return result, nil
}

// refresh 同步加载全量成员到缓存。超过 maxCacheSize 时清理过期条目。
func (mc *memberCache) refresh(db *dbr.Session, spaceID string) error {
	var allUIDs []string
	_, err := db.Select("uid").From("space_member").
		Where("space_id = ? AND status = 1", spaceID).
		Load(&allUIDs)
	if err != nil {
		return err
	}

	uidSet := make(map[string]bool, len(allUIDs))
	for _, uid := range allUIDs {
		uidSet[uid] = true
	}

	mc.mu.Lock()
	if len(mc.entries) >= maxCacheSize {
		now := time.Now()
		for k, v := range mc.entries {
			if now.After(v.expireAt) {
				delete(mc.entries, k)
			}
		}
	}
	mc.entries[spaceID] = &memberCacheEntry{
		uids:     uidSet,
		expireAt: time.Now().Add(cacheTTL),
	}
	mc.mu.Unlock()
	return nil
}

// invalidate 删除指定 Space 的缓存。
func (mc *memberCache) invalidate(spaceID string) {
	mc.mu.Lock()
	delete(mc.entries, spaceID)
	mc.mu.Unlock()
}
