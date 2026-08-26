// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package packages_test

import (
	"reflect"
	"strings"
	"testing"

	packages_model "gitea.dev/models/packages"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// genPathSegment yields realistic container path segments (Docker Hub namespaces, library names
// and first-party image names) so generated prefixes and images look like the real inputs.
func genPathSegment() gopter.Gen {
	return gen.OneConstOf(
		"nats", "mongo", "redis", "alpine", "busybox",
		"library", "noenv", "ai-platform", "ai-playground", "5gsystems",
		"core-app", "proxy-lab",
	)
}

// genPath yields a slash-joined path of 1..maxSegments segments, e.g. "nats" or "noenv/nats".
func genPath(maxSegments int) gopter.Gen {
	return gen.IntRange(1, maxSegments).
		FlatMap(func(n interface{}) gopter.Gen {
			return gen.SliceOfN(n.(int), genPathSegment())
		}, reflect.TypeOf([]string{})).
		Map(func(segments []string) string {
			return strings.Join(segments, "/")
		})
}

// genRemotePrefix yields the empty prefix (org-name == namespace default) as well as single- and
// multi-segment prefixes, so both branches of UpstreamAddr are exercised.
func genRemotePrefix() gopter.Gen {
	return gen.OneGenOf(
		gen.Const(""),
		genPath(1),
		genPath(2),
	)
}

// Feature: proxy-registry-admin-and-orgs, Property 4: Remote-prefix mapping composes the upstream path
//
// For any container upstream with remote prefix `p` and any image `image`, the upstream request
// address SHALL equal `image` when `p` is empty and `p + "/" + image` otherwise, so the upstream
// /v2/<addr>/... URL and the Bearer scope target the correct Docker Hub namespace.
//
// **Validates: Requirements 6.4**
//
// UpstreamAddr does no trimming or slash normalization, so this asserts the documented
// composition literally: exactly one separator is introduced, nothing else is rewritten.
func TestPropertyUpstreamAddrComposesRemotePrefix(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100

	properties := gopter.NewProperties(params)

	properties.Property("UpstreamAddr prepends RemotePrefix with a single separator", prop.ForAll(
		func(prefix, image string) string {
			up := &packages_model.PackageRegistryUpstream{RemotePrefix: prefix}
			got := up.UpstreamAddr(image)

			if prefix == "" {
				if got != image {
					return "empty RemotePrefix must pass the image through unchanged, got " + got
				}
				return ""
			}

			if want := prefix + "/" + image; got != want {
				return "expected " + want + ", got " + got
			}
			// Structural invariants of the composition: the address is the image located below
			// the prefix, with exactly one separator added and neither part rewritten.
			if !strings.HasPrefix(got, prefix+"/") {
				return "address must start with the remote prefix, got " + got
			}
			if !strings.HasSuffix(got, "/"+image) {
				return "address must end with the image path, got " + got
			}
			if strings.Count(got, "/") != strings.Count(prefix, "/")+strings.Count(image, "/")+1 {
				return "composition must add exactly one separator, got " + got
			}
			return ""
		},
		genRemotePrefix(),
		genPath(3),
	))

	properties.TestingRun(t)
}
