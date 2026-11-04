package tokenize

import (
	"hash/maphash"
	"sync"
)

// countCache is a sharded LRU of token counts by text content (maphash of
// the text plus its length; the text itself is not kept). Load generators
// repeat prompts, so the BPE runs once per distinct message.
type countCache struct {
	seed   maphash.Seed
	shards [cacheShards]cacheShard
}

const cacheShards = 16

type cacheShard struct {
	mu    sync.Mutex
	index map[cacheKey]int32
	items []cacheItem // fixed capacity; the list is doubly linked by index
	head  int32       // most recently used, -1 when empty
	tail  int32       // least recently used
}

type cacheKey struct {
	hash uint64
	size int
}

type cacheItem struct {
	key        cacheKey
	count      int
	prev, next int32
}

func newCountCache(entries int) *countCache {
	c := &countCache{seed: maphash.MakeSeed()}
	per := max(entries/cacheShards, 1)
	for i := range c.shards {
		c.shards[i] = cacheShard{index: make(map[cacheKey]int32, per), items: make([]cacheItem, 0, per), head: -1, tail: -1}
	}
	return c
}

func (c *countCache) key(text string) (cacheKey, *cacheShard) {
	h := maphash.String(c.seed, text)
	return cacheKey{h, len(text)}, &c.shards[h%cacheShards]
}

func (c *countCache) get(text string) (int, bool) {
	k, s := c.key(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.index[k]
	if !ok {
		return 0, false
	}
	s.touch(i)
	return s.items[i].count, true
}

func (c *countCache) put(text string, count int) {
	k, s := c.key(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.index[k]; ok {
		s.items[i].count = count
		s.touch(i)
		return
	}
	var i int32
	if len(s.items) < cap(s.items) {
		i = int32(len(s.items)) //nolint:gosec // G115: below the shard capacity
		s.items = append(s.items, cacheItem{prev: -1, next: -1})
	} else {
		i = s.tail // evict the least recently used
		s.unlink(i)
		delete(s.index, s.items[i].key)
	}
	s.items[i].key, s.items[i].count = k, count
	s.index[k] = i
	s.pushFront(i)
}

func (s *cacheShard) touch(i int32) {
	if s.head == i {
		return
	}
	s.unlink(i)
	s.pushFront(i)
}

func (s *cacheShard) unlink(i int32) {
	it := &s.items[i]
	if it.prev >= 0 {
		s.items[it.prev].next = it.next
	} else {
		s.head = it.next
	}
	if it.next >= 0 {
		s.items[it.next].prev = it.prev
	} else {
		s.tail = it.prev
	}
	it.prev, it.next = -1, -1
}

func (s *cacheShard) pushFront(i int32) {
	it := &s.items[i]
	it.prev, it.next = -1, s.head
	if s.head >= 0 {
		s.items[s.head].prev = i
	}
	s.head = i
	if s.tail < 0 {
		s.tail = i
	}
}
