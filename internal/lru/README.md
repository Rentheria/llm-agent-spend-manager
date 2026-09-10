# internal/lru

In-memory LRU (Least Recently Used) cache implementation for tracking recent sessions and requests.

## Structure

- **Map**: `id → node` for O(1) lookup
- **Doubly-linked list**: head = MRU (most recently used), tail = LRU (least recently used)
- **Operations**: All O(1) - Get/Touch, Put, Evict, Delete

## Configuration

The cache capacity is configured via the `LASM_LRU_CAPACITY` environment variable:

```bash
export LASM_LRU_CAPACITY=500
```

**Default**: 1000 entries if not set or if the value is invalid.

## Usage

### Generic LRU Cache

```go
import "github.com/Rentheria/llm-agent-spend-manager/internal/lru"

// Create a cache with capacity 100
cache := lru.New(100)

// Put a value
cache.Put("key1", "value1")

// Get a value (marks as recently used)
if val, ok := cache.Get("key1"); ok {
    fmt.Println("Found:", val)
}

// Touch a key (mark as recently used without retrieving value)
cache.Touch("key1")

// Delete a key
cache.Delete("key1")

// Get all keys in MRU-to-LRU order
keys := cache.Keys()
```

### Request Cache (Domain-Specific)

The `RequestCache` wraps the generic LRU cache for tracking request metadata:

```go
import "github.com/Rentheria/llm-agent-spend-manager/internal/lru"

// Create from environment variable
cache := lru.NewRequestCacheFromEnv()

// Or with explicit capacity
cache := lru.NewRequestCache(500)

// Record a request
cache.Record(lru.RequestInfo{
    Key:        "req-123",
    Timestamp:  time.Now(),
    TokenCount: 1500,
    Model:      "claude-opus-4",
    Agent:      "Claude Code",
})

// Lookup request info
if info, ok := cache.Lookup("req-123"); ok {
    fmt.Printf("Request: %s at %v\n", info.Key, info.Timestamp)
}

// Get recent requests (MRU to LRU)
recent := cache.Recent()

// Get cache statistics
hits, misses, size := cache.Stats()
fmt.Printf("Cache: %d hits, %d misses, %d entries\n", hits, misses, size)
```

## Integration with Proxy

The LRU cache is optionally integrated into the proxy for tracking recent request metadata:

```go
import (
    "github.com/Rentheria/llm-agent-spend-manager/internal/proxy"
    "github.com/Rentheria/llm-agent-spend-manager/internal/lru"
)

// Create request cache
cache := lru.NewRequestCacheFromEnv()

// Create proxy with request caching enabled
p, err := proxy.New(
    "https://api.anthropic.com",
    limiter,
    proxy.WithRequestCache(cache),
)
```

When enabled, the proxy records metadata about each forwarded request (timestamp, key, agent, model) in the cache for fast lookup. This is useful for:
- Request deduplication
- Rate limiting analysis
- Debugging recent traffic patterns
- Session tracking

## When Used

The LRU cache is used when:
1. The proxy is created with `WithRequestCache()` option
2. The cache is explicitly created and used by other components for session/request tracking

## Thread Safety

All cache operations are thread-safe and can be called concurrently from multiple goroutines.

## Testing

Run the tests:

```bash
go test ./internal/lru/... -v
```

All operations are tested including:
- Basic put/get operations
- LRU eviction behavior
- Touch operation (moves to head)
- Delete operations (head, tail, middle)
- Order preservation
- Concurrent access
