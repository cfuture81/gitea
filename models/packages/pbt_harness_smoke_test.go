// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package packages_test

import (
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// Feature: proxy-registry-admin-and-orgs, Property 0: PBT harness smoke check
//
// This is NOT one of the design's ten correctness properties (P1-P10) and it validates no
// acceptance criterion. It exists only to prove the property-based testing harness is wired
// up and executing, and to establish the conventions every real property test follows:
//
//   - generators come from the library (github.com/leanovate/gopter/gen), never hand-rolled;
//   - MinSuccessfulTests is set explicitly to at least 100 iterations;
//   - the test carries a `// Feature: <feature>, Property <n>: <property text>` comment.
//
// Real property tests replace the trivial round-trip below with a design property and add a
// `**Validates: Requirements X.Y**` marker, which this harness check deliberately omits.
func TestPBTHarnessSmoke(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100

	properties := gopter.NewProperties(params)

	properties.Property("a string round-trips through a rune conversion", prop.ForAll(
		func(s string) bool {
			return string([]rune(s)) == s
		},
		gen.AnyString(),
	))

	properties.TestingRun(t)
}
