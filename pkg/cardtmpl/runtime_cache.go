package cardtmpl

import (
	"container/list"
	"sync"
)

type compiledCacheEntry struct {
	key      string
	artifact *CompiledArtifact
	bytes    int
}

type compiledArtifactCache struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int
	bytes      int
	lru        *list.List
	entries    map[string]*list.Element
}

func newCompiledArtifactCache(maxEntries, maxBytes int) *compiledArtifactCache {
	return &compiledArtifactCache{
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		lru:        list.New(),
		entries:    make(map[string]*list.Element, maxEntries),
	}
}

func (c *compiledArtifactCache) get(key string) (*CompiledArtifact, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(element)
	return element.Value.(*compiledCacheEntry).artifact, true
}

func (c *compiledArtifactCache) add(key string, artifact *CompiledArtifact) int {
	if c == nil || artifact == nil {
		return 0
	}
	weight := len(artifact.Bundle)
	if weight <= 0 || weight > c.maxBytes {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[key]; ok {
		entry := existing.Value.(*compiledCacheEntry)
		c.bytes -= entry.bytes
		entry.artifact = artifact
		entry.bytes = weight
		c.bytes += weight
		c.lru.MoveToFront(existing)
	} else {
		entry := &compiledCacheEntry{key: key, artifact: artifact, bytes: weight}
		element := c.lru.PushFront(entry)
		c.entries[key] = element
		c.bytes += weight
	}
	evicted := 0
	for len(c.entries) > c.maxEntries || c.bytes > c.maxBytes {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*compiledCacheEntry)
		delete(c.entries, entry.key)
		c.bytes -= entry.bytes
		c.lru.Remove(oldest)
		evicted++
	}
	return evicted
}

func (c *compiledArtifactCache) size() (entries, bytes int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.bytes
}
