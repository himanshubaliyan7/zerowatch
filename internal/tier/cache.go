package tier

import (
	"container/list"
	"sync"
)

// lru is a byte-bounded LRU cache for manifests and segment blocks.
type lru struct {
	mu       sync.Mutex
	max, cur int
	ll       *list.List
	items    map[string]*list.Element
	hits     int64
	misses   int64
}

type lruEntry struct {
	key  string
	val  any
	size int
}

func newLRU(maxBytes int) *lru {
	return &lru{max: maxBytes, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *lru) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.ll.MoveToFront(e)
		c.hits++
		return e.Value.(*lruEntry).val, true
	}
	c.misses++
	return nil, false
}

// put stores val; values larger than the whole cache are not kept.
func (c *lru) put(key string, val any, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.cur -= e.Value.(*lruEntry).size
		c.ll.Remove(e)
		delete(c.items, key)
	}
	if size > c.max {
		return
	}
	c.items[key] = c.ll.PushFront(&lruEntry{key, val, size})
	c.cur += size
	for c.cur > c.max {
		e := c.ll.Back()
		ent := e.Value.(*lruEntry)
		c.ll.Remove(e)
		delete(c.items, ent.key)
		c.cur -= ent.size
	}
}

func (c *lru) remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.cur -= e.Value.(*lruEntry).size
		c.ll.Remove(e)
		delete(c.items, key)
	}
}

// removePrefix drops every key starting with p.
func (c *lru) removePrefix(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.items {
		if len(k) >= len(p) && k[:len(p)] == p {
			c.cur -= e.Value.(*lruEntry).size
			c.ll.Remove(e)
			delete(c.items, k)
		}
	}
}

func (c *lru) stats() (bytes int, hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur, c.hits, c.misses
}
