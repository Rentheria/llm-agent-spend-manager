package lru

import (
	"sync/atomic"
	"time"
)

// RequestInfo holds metadata about a proxied request.
type RequestInfo struct {
	Key        string
	Timestamp  time.Time
	TokenCount int64
	Model      string
	Agent      string
}

// RequestCache is a specialized LRU cache for tracking recent requests/sessions.
// It wraps the generic LRU cache and provides domain-specific operations for
// request tracking in the proxy.
type RequestCache struct {
	cache *Cache
	hits  atomic.Int64
	misses atomic.Int64
}

// NewRequestCache creates a request cache with the given capacity.
func NewRequestCache(capacity int) *RequestCache {
	return &RequestCache{
		cache: New(capacity),
	}
}

// NewRequestCacheFromEnv creates a request cache using the capacity from
// the environment variable LASM_LRU_CAPACITY, or the default if not set.
func NewRequestCacheFromEnv() *RequestCache {
	return NewRequestCache(LoadCapacity())
}

// Record stores information about a request.
func (rc *RequestCache) Record(info RequestInfo) {
	rc.cache.Put(info.Key, info)
}

// Lookup retrieves request info by key.
// Returns (info, true) if found, (zero, false) otherwise.
func (rc *RequestCache) Lookup(key string) (RequestInfo, bool) {
	val, ok := rc.cache.Get(key)
	if !ok {
		rc.misses.Add(1)
		return RequestInfo{}, false
	}
	rc.hits.Add(1)
	if info, ok := val.(RequestInfo); ok {
		return info, true
	}
	return RequestInfo{}, false
}

// Touch marks a request as recently accessed without retrieving its full info.
func (rc *RequestCache) Touch(key string) bool {
	return rc.cache.Touch(key)
}

// Recent returns all cached request keys in MRU-to-LRU order.
func (rc *RequestCache) Recent() []string {
	return rc.cache.Keys()
}

// Stats returns the current hit count, miss count, and cache size.
func (rc *RequestCache) Stats() (hits, misses int64, size int) {
	return rc.hits.Load(), rc.misses.Load(), rc.cache.Len()
}

// Len returns the current number of cached requests.
func (rc *RequestCache) Len() int {
	return rc.cache.Len()
}

// Capacity returns the maximum number of requests this cache can hold.
func (rc *RequestCache) Capacity() int {
	return rc.cache.Capacity()
}

// Clear removes all entries and resets statistics.
func (rc *RequestCache) Clear() {
	rc.cache.Clear()
	rc.hits.Store(0)
	rc.misses.Store(0)
}
