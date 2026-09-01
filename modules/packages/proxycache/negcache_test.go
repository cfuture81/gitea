// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package proxycache

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// resetCache empties the process-local negative cache so tests do not leak state into each other.
func resetCache(t *testing.T) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	blocked = map[string]time.Time{}
}

func TestClearUnblocksKey(t *testing.T) {
	resetCache(t)

	key := "1|library/nats|latest"
	Block(key, time.Hour)
	assert.True(t, Blocked(key), "key should be blocked after Block")

	Clear(key)
	assert.False(t, Blocked(key), "key should not be blocked after Clear")
}

func TestClearLeavesOtherKeysIntact(t *testing.T) {
	resetCache(t)

	target := "1|library/nats|latest"
	other := "1|library/nats|v2"
	Block(target, time.Hour)
	Block(other, time.Hour)

	Clear(target)

	assert.False(t, Blocked(target))
	assert.True(t, Blocked(other), "unrelated key must stay blocked")
}

func TestClearMissingKeyIsNoOp(t *testing.T) {
	resetCache(t)

	kept := "1|library/nats|latest"
	Block(kept, time.Hour)

	assert.NotPanics(t, func() { Clear("does|not|exist") })
	assert.False(t, Blocked("does|not|exist"))
	assert.True(t, Blocked(kept), "clearing an absent key must not touch existing entries")
}

func TestClearPrefixClearsMatchingEntriesOnly(t *testing.T) {
	resetCache(t)

	// same upstream + image, several references -> all must be cleared
	matching := []string{
		"7|library/nats|latest",
		"7|library/nats|2.10",
		"7|library/nats|sha256:abc",
	}
	// different reference space: other image, other upstream, and a same-named image
	// under an upstream whose ID merely shares a digit prefix.
	nonMatching := []string{
		"7|library/redis|latest",
		"8|library/nats|latest",
		"70|library/nats|latest",
	}
	for _, k := range append(append([]string{}, matching...), nonMatching...) {
		Block(k, time.Hour)
	}

	ClearPrefix("7|library/nats|")

	for _, k := range matching {
		assert.Falsef(t, Blocked(k), "%s should have been cleared by ClearPrefix", k)
	}
	for _, k := range nonMatching {
		assert.Truef(t, Blocked(k), "%s must be left intact by ClearPrefix", k)
	}
}

func TestClearPrefixWithNoMatchIsNoOp(t *testing.T) {
	resetCache(t)

	kept := "7|library/nats|latest"
	Block(kept, time.Hour)

	assert.NotPanics(t, func() { ClearPrefix("9|library/other|") })
	assert.True(t, Blocked(kept))
}

func TestShouldNegativeCache(t *testing.T) {
	assert.True(t, ShouldNegativeCache(http.StatusNotFound))
	assert.True(t, ShouldNegativeCache(http.StatusGone))
	for _, s := range []int{0, 200, 301, 401, 403, 429, 500, 502, 503, 504} {
		assert.False(t, ShouldNegativeCache(s), "status %d must not be negatively cached", s)
	}
}

func TestBlockedSemanticsUnchanged(t *testing.T) {
	resetCache(t)

	// unknown key is not blocked
	assert.False(t, Blocked("1|library/nats|latest"))

	// ttl below the 60s floor is raised, so the entry is still blocked immediately after
	Block("1|library/nats|latest", time.Millisecond)
	assert.True(t, Blocked("1|library/nats|latest"))
}
