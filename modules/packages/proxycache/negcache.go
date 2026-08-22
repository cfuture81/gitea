// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package proxycache provides a small process-local negative cache for upstream package proxies:
// when an upstream returns "not found" for a path, we remember it briefly so repeated client
// misses don't hammer the upstream (mirrors Nexus's per-proxy negative cache).
package proxycache

import (
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	blocked = map[string]time.Time{}
)

// Blocked reports whether key is currently negatively cached (a recent upstream miss).
func Blocked(key string) bool {
	mu.Lock()
	defer mu.Unlock()
	exp, ok := blocked[key]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(blocked, key)
		return false
	}
	return true
}

// Block negatively caches key for ttl (a floor of 60s is applied; non-positive ttl uses 60s).
func Block(key string, ttl time.Duration) {
	if ttl < 60*time.Second {
		ttl = 60 * time.Second
	}
	mu.Lock()
	defer mu.Unlock()
	blocked[key] = time.Now().Add(ttl)
	// opportunistic cleanup of a few expired entries to bound growth
	if len(blocked) > 4096 {
		now := time.Now()
		for k, exp := range blocked {
			if now.After(exp) {
				delete(blocked, k)
			}
		}
	}
}
