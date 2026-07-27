package render

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
)

// Cache memoises the verdict for an identical (chart, values) render.
//
// It stores only the verdict - manifest count, byte count, digest - never the
// rendered YAML. A bulk request that hits the cache wants to know "does this
// render and is it the same as last time", and holding the payloads for a
// 55 MB library would cost more than the renders it avoids.
type Cache interface {
	Get(key string) (ReleaseResult, bool)
	Put(key string, val ReleaseResult)
	Stats() CacheStats
}

// CacheStats reports hit rate, which is the only number that decides whether
// the cache is worth its lookup cost.
type CacheStats struct {
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
	Entries int   `json:"entries"`
	Evicted int64 `json:"evicted"`
}

const cacheShards = 32

// ShardedCache is a bounded, sharded map. Eviction is by shard flush rather
// than LRU: tracking recency costs a write on every read, which under this
// workload (short-lived bulk batches, no long tail of reuse) buys nothing.
type ShardedCache struct {
	shards   [cacheShards]cacheShard
	perShard int
	hits     atomic.Int64
	misses   atomic.Int64
	evicted  atomic.Int64
}

type cacheShard struct {
	mu sync.RWMutex
	m  map[string]ReleaseResult
}

// NewCache returns a cache holding roughly capacity entries in total.
func NewCache(capacity int) *ShardedCache {
	if capacity < cacheShards {
		capacity = cacheShards
	}
	c := &ShardedCache{perShard: capacity / cacheShards}
	for i := range c.shards {
		c.shards[i].m = make(map[string]ReleaseResult, c.perShard)
	}
	return c
}

func (c *ShardedCache) shard(key string) *cacheShard {
	// Keys are already hex-encoded hashes, so the low bits are uniform.
	var idx uint8
	if len(key) > 0 {
		idx = key[len(key)-1]
	}
	return &c.shards[idx%cacheShards]
}

// Get returns a cached verdict.
func (c *ShardedCache) Get(key string) (ReleaseResult, bool) {
	s := c.shard(key)
	s.mu.RLock()
	v, ok := s.m[key]
	s.mu.RUnlock()
	if ok {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
	return v, ok
}

// Put stores a verdict, flushing the shard if it has outgrown its budget.
func (c *ShardedCache) Put(key string, val ReleaseResult) {
	s := c.shard(key)
	s.mu.Lock()
	if len(s.m) >= c.perShard {
		c.evicted.Add(int64(len(s.m)))
		s.m = make(map[string]ReleaseResult, c.perShard)
	}
	s.m[key] = val
	s.mu.Unlock()
}

// Reset empties the cache and zeroes the counters. Benchmarks need it to
// measure a cold batch; without it a repeated batch measures nothing but the
// cache hitting itself.
func (c *ShardedCache) Reset() {
	for i := range c.shards {
		c.shards[i].mu.Lock()
		c.shards[i].m = make(map[string]ReleaseResult, c.perShard)
		c.shards[i].mu.Unlock()
	}
	c.hits.Store(0)
	c.misses.Store(0)
	c.evicted.Store(0)
}

// Stats returns hit/miss counters.
func (c *ShardedCache) Stats() CacheStats {
	st := CacheStats{Hits: c.hits.Load(), Misses: c.misses.Load(), Evicted: c.evicted.Load()}
	for i := range c.shards {
		c.shards[i].mu.RLock()
		st.Entries += len(c.shards[i].m)
		c.shards[i].mu.RUnlock()
	}
	return st
}

// cacheKey hashes the full render input. It walks the values tree directly
// into the hash rather than marshalling to JSON first, because the whole point
// of the cache is to be cheaper than the render it replaces - and a JSON
// round-trip of a deep values map is not obviously cheaper than a small chart.
func cacheKey(chart, release, namespace string, vals map[string]any) string {
	h := sha256.New()
	h.Write([]byte(chart))
	h.Write([]byte{0})
	h.Write([]byte(release))
	h.Write([]byte{0})
	h.Write([]byte(namespace))
	h.Write([]byte{0})
	hashValue(h, vals)
	return hex.EncodeToString(h.Sum(nil))
}

// hashValue writes a canonical encoding of v into h. Map keys are sorted so
// that two equal maps always hash the same regardless of iteration order.
func hashValue(h hash.Hash, v any) {
	switch t := v.(type) {
	case nil:
		h.Write([]byte{'n'})
	case map[string]any:
		h.Write([]byte{'{'})
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{':'})
			hashValue(h, t[k])
			h.Write([]byte{','})
		}
		h.Write([]byte{'}'})
	case []any:
		h.Write([]byte{'['})
		for _, e := range t {
			hashValue(h, e)
			h.Write([]byte{','})
		}
		h.Write([]byte{']'})
	case string:
		h.Write([]byte{'s'})
		h.Write([]byte(t))
	case bool:
		if t {
			h.Write([]byte{'T'})
		} else {
			h.Write([]byte{'F'})
		}
	case int:
		h.Write([]byte{'i'})
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(t))
		h.Write(buf[:])
	case int64:
		h.Write([]byte{'i'})
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(t))
		h.Write(buf[:])
	case float64:
		h.Write([]byte{'f'})
		h.Write([]byte(strconv.FormatFloat(t, 'g', -1, 64)))
	default:
		h.Write([]byte{'?'})
		fmt.Fprintf(h, "%v", t)
	}
}
