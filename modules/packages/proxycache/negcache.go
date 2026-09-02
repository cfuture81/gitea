// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package proxycache provides a small process-local negative cache for upstream package proxies:
// when an upstream returns "not found" for a path, we remember it briefly so repeated client
// misses don't hammer the upstream (mirrors Nexus's per-proxy negative cache).
package proxycache

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	blocked = map[string]time.Time{}
)

// UserAgent is sent on all outbound package-proxy upstream requests. Some upstreams (notably
// Maven Central / Fastly) throttle the default Go client UA ("Go-http-client/*") with HTTP 429,
// so a descriptive UA is required.
const UserAgent = "Gitea-Package-Proxy/1.0"

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

// NegativeCacheTTL is the lifetime of a definitive upstream "not found" negative-cache entry.
// Kept short so a transient upstream miss never blocks a real artifact for long (Block also
// applies a 60s floor).
const NegativeCacheTTL = 60 * time.Second

// ShouldNegativeCache reports whether an upstream HTTP status is a DEFINITIVE "not found"
// (404/410) that may be briefly negatively cached. Transient failures — transport errors
// (represented as status 0), timeouts, 5xx, 429, auth — MUST NOT be cached, so an upstream
// blip never poisons a real artifact. Mirrors Nexus's split between the per-artifact
// "not found" cache (404) and remote auto-block (transient/5xx).
func ShouldNegativeCache(status int) bool {
	return status == http.StatusNotFound || status == http.StatusGone
}

// Clear removes the negative-cache entry for key so the next lookup is no longer suppressed.
// It is a no-op when key is not cached. Used when locally cached content is deleted and a fresh
// upstream fetch must be allowed before the original ttl elapses.
func Clear(key string) {
	mu.Lock()
	defer mu.Unlock()
	delete(blocked, key)
}

// ClearPrefix removes every negative-cache entry whose key starts with prefix, unblocking all
// entries cached under it (e.g. "<upstreamID>|<image>|" clears every reference of that
// upstream+image). An empty prefix clears the whole cache.
func ClearPrefix(prefix string) {
	mu.Lock()
	defer mu.Unlock()
	for k := range blocked {
		if strings.HasPrefix(k, prefix) {
			delete(blocked, k)
		}
	}
}
