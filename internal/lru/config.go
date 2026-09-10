package lru

import (
	"os"
	"strconv"
)

// DefaultCapacity is used when LASM_LRU_CAPACITY is not set or invalid.
const DefaultCapacity = 1000

// EnvCapacity is the environment variable name for configuring cache capacity.
const EnvCapacity = "LASM_LRU_CAPACITY"

// LoadCapacity reads the cache capacity from the environment or returns the default.
// Invalid values (non-positive integers) fall back to the default.
func LoadCapacity() int {
	raw := os.Getenv(EnvCapacity)
	if raw == "" {
		return DefaultCapacity
	}
	cap, err := strconv.Atoi(raw)
	if err != nil || cap <= 0 {
		return DefaultCapacity
	}
	return cap
}
