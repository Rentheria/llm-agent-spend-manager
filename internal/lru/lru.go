// Package lru provides an in-memory LRU cache backed by a doubly-linked list.
//
// Structure: map[string]*node + doubly linked list (head=MRU, tail=LRU).
// Operations: O(1) Get/Touch, Put, Evict, Delete.
//
// Use case: retain recent session/request context in memory without hitting
// disk or database on every lookup. The cache size is configurable via the
// LASM_LRU_CAPACITY environment variable (default 1000).
package lru

import (
	"sync"
)

// node is one entry in the doubly-linked list.
type node struct {
	key   string
	value interface{}
	prev  *node
	next  *node
}

// Cache is an LRU cache with O(1) operations.
//
// The cache maintains a doubly-linked list where the head is the most recently
// used item and the tail is the least recently used item. A map provides O(1)
// access to any node by key.
type Cache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*node
	head     *node // MRU (most recently used)
	tail     *node // LRU (least recently used)
}

// New creates an LRU cache with the specified capacity.
// Capacity must be positive.
func New(capacity int) *Cache {
	if capacity <= 0 {
		capacity = 1
	}
	return &Cache{
		capacity: capacity,
		items:    make(map[string]*node, capacity),
	}
}

// Get retrieves the value associated with key and marks it as recently used.
// Returns (value, true) if found, (nil, false) otherwise.
func (c *Cache) Get(key string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.moveToHead(n)
	return n.value, true
}

// Put inserts or updates a key-value pair and marks it as recently used.
// If the cache is at capacity, the least recently used item is evicted first.
func (c *Cache) Put(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if n, ok := c.items[key]; ok {
		// Update existing node
		n.value = value
		c.moveToHead(n)
		return
	}

	// Create new node
	n := &node{key: key, value: value}
	c.items[key] = n
	c.addToHead(n)

	// Evict LRU if over capacity
	if len(c.items) > c.capacity {
		c.evictTail()
	}
}

// Touch marks a key as recently used without retrieving its value.
// Returns true if the key exists, false otherwise.
func (c *Cache) Touch(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, ok := c.items[key]
	if !ok {
		return false
	}
	c.moveToHead(n)
	return true
}

// Delete removes a key from the cache.
// Returns true if the key was present, false otherwise.
func (c *Cache) Delete(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, ok := c.items[key]
	if !ok {
		return false
	}
	c.removeNode(n)
	delete(c.items, key)
	return true
}

// Len returns the current number of items in the cache.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Clear removes all items from the cache.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*node, c.capacity)
	c.head = nil
	c.tail = nil
}

// Capacity returns the maximum number of items the cache can hold.
func (c *Cache) Capacity() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacity
}

// Keys returns all keys in the cache in MRU-to-LRU order.
// The returned slice is a snapshot; modifications to it do not affect the cache.
func (c *Cache) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	keys := make([]string, 0, len(c.items))
	for n := c.head; n != nil; n = n.next {
		keys = append(keys, n.key)
	}
	return keys
}

// moveToHead moves an existing node to the head (MRU position).
// Caller must hold the lock.
func (c *Cache) moveToHead(n *node) {
	if n == c.head {
		return
	}
	c.removeNode(n)
	c.addToHead(n)
}

// addToHead adds a node to the head of the list.
// Caller must hold the lock.
func (c *Cache) addToHead(n *node) {
	n.next = c.head
	n.prev = nil

	if c.head != nil {
		c.head.prev = n
	}
	c.head = n

	if c.tail == nil {
		c.tail = n
	}
}

// removeNode removes a node from the list.
// Caller must hold the lock.
func (c *Cache) removeNode(n *node) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		c.head = n.next
	}

	if n.next != nil {
		n.next.prev = n.prev
	} else {
		c.tail = n.prev
	}

	n.prev = nil
	n.next = nil
}

// evictTail removes the least recently used item.
// Caller must hold the lock.
func (c *Cache) evictTail() {
	if c.tail == nil {
		return
	}
	old := c.tail
	c.removeNode(old)
	delete(c.items, old.key)
}
