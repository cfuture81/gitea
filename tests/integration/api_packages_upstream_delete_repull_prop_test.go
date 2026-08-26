// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitea.dev/models/organization"
	packages_model "gitea.dev/models/packages"
	container_model "gitea.dev/models/packages/container"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/packages/proxycache"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/structs"
	"gitea.dev/modules/test"
	packages_service "gitea.dev/services/packages"
	"gitea.dev/tests"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// genRepullImageBase yields realistic single-segment container image names.
func genRepullImageBase() gopter.Gen {
	return gen.OneConstOf("nats", "mongo", "redis", "alpine", "busybox", "core-app", "wallet-sync")
}

// genRepullTag yields lowercase references within the container reference pattern, so the tag, the
// stored LowerVersion and the negative-cache key segment are all the same string.
func genRepullTag() gopter.Gen {
	return gen.OneConstOf("latest", "main", "stable", "1.0", "v2.1.3", "20260101")
}

// genRepullArchCount decides the manifest-graph shape: 0 is a plain image manifest, 1..3 an OCI
// index with that many per-arch children. Multi-arch matters here because the delete has to cascade
// to the orphaned children and the re-pull has to re-fetch the whole graph.
func genRepullArchCount() gopter.Gen {
	return gen.IntRange(0, 3)
}

// Feature: proxy-registry-admin-and-orgs, Property 8: Delete then re-pull round-trip
//
// For any proxied container version, after deletion no cached representation of that version SHALL
// remain locally, and a subsequent pull SHALL re-fetch it from the configured upstream and mark the
// re-created version as proxied (`upstream.cached`), never as internal.
//
// **Validates: Requirements 8.2, 8.3, 8.4**
//
// This is the executable proof of the reported bug: a Docker-Hub-cached version that was deleted
// reappeared marked `internal` instead of being re-fetched. Each iteration walks the full
// round-trip and asserts every step, so any single regression along it fails the property:
//
//  1. cold proxy-pull - the fake upstream's request counter must move (a warm iteration would make
//     the whole round-trip vacuous), and the created version must carry
//     `upstream.cached` + `upstream.source`;
//  2. the negative-cache entry for exactly this upstream+image+reference is then poisoned on
//     purpose, standing in for a transient upstream failure that happened for this reference - it
//     is the state in which a delete that forgets to unblock the key silently suppresses the next
//     pull for the remainder of MetadataTTL (floored at 60s, so it cannot be waited out);
//  3. delete through RemoveProxiedVersionAndOrphans (the service the OCI route, the web UI, the
//     admin list and the v1 API all funnel into) - after it, the version row, the container
//     manifest blob AND every orphaned per-arch child must be gone, and the poisoned
//     negative-cache key must be cleared;
//  4. re-pull - the counter must increase AGAIN. That is the assertion that separates a real
//     re-fetch from a warm cache hit or a suppressed pull; and
//  5. the re-created version must again carry `upstream.cached`/`upstream.source` - the source
//     badge renders "internal" precisely when `upstream.cached` is absent, so this is the
//     regression check for the reported symptom.
//
// Hermetic: the upstream is an in-process httptest server; no external network. One environment,
// one fake upstream and one token are shared across iterations, and every iteration uses a unique
// image name plus nonce-derived digests so its cold pull is genuinely cold.
func TestPropertyUpstreamDeleteThenRepull(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	upstream := newColdWarmUpstream()
	ts := httptest.NewServer(upstream)
	defer ts.Close()

	org := &organization.Organization{
		Name:       "noenv",
		IsActive:   true,
		Type:       user_model.UserTypeOrganization,
		Visibility: structs.VisibleTypePublic,
	}
	require.NoError(t, organization.CreateOrganization(t.Context(), org, owner))

	up, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		TargetOwnerID: org.ID, Type: packages_model.TypeContainer, Name: "dockerhub", URL: ts.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 100, IsAdminManaged: true,
	})
	require.NoError(t, err)

	var tok struct {
		Token string `json:"token"`
	}
	resp := MakeRequest(t, NewRequest(t, "GET", setting.AppURL+"v2/token").AddBasicAuth(owner.Name), http.StatusOK)
	DecodeJSON(t, resp, &tok)
	userToken := "Bearer " + tok.Token

	// requireProxied returns "" when the version exists under the org and is marked as proxy cache
	// content, or a description of what is wrong otherwise. "not internal" is exactly "carries
	// upstream.cached": templates/package/shared/source_badge.tmpl renders the green "internal"
	// badge in the else branch of that property.
	requireProxied := func(image, tag, phase string) string {
		pv, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID,
			packages_model.TypeContainer, strings.ToLower(image), strings.ToLower(tag))
		if err != nil {
			return fmt.Sprintf("%s: no version %s:%s under org %q: %v", phase, image, tag, org.Name, err)
		}
		cached, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, pv.ID, packages_model.PropertyUpstreamCached)
		if err != nil {
			return fmt.Sprintf("%s: reading %s of %s:%s: %v", phase, packages_model.PropertyUpstreamCached, image, tag, err)
		}
		if len(cached) != 1 || cached[0].Value != "1" {
			return fmt.Sprintf("%s: %s:%s carries %d %s properties, want exactly one with value \"1\" - without it the UI renders the version as first-party \"internal\"",
				phase, image, tag, len(cached), packages_model.PropertyUpstreamCached)
		}
		source, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, pv.ID, packages_model.PropertyUpstreamSource)
		if err != nil {
			return fmt.Sprintf("%s: reading %s of %s:%s: %v", phase, packages_model.PropertyUpstreamSource, image, tag, err)
		}
		if len(source) != 1 || source[0].Value != up.Name {
			return fmt.Sprintf("%s: %s:%s carries %s = %v, want exactly [%q]", phase, image, tag, packages_model.PropertyUpstreamSource, source, up.Name)
		}
		return ""
	}

	var iteration atomic.Int64
	var refetches atomic.Int64

	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100

	properties := gopter.NewProperties(params)

	properties.Property("a deleted proxied version leaves nothing cached and is re-fetched, marked proxied, on the next pull", prop.ForAll(
		func(imageBase, tag string, arches int) string {
			nonce := "i" + strconv.FormatInt(iteration.Add(1), 10)
			image := imageBase + "-" + nonce
			img := buildColdWarmImage(nonce, arches)
			upstream.publish(image, tag, img)

			pullURL := fmt.Sprintf("%sv2/%s/%s/manifests/%s", setting.AppURL, org.Name, image, tag)
			pull := func() *httptest.ResponseRecorder {
				return MakeRequest(t, NewRequest(t, "GET", pullURL).
					AddTokenAuth(userToken).SetHeader("Accept", img.topType), NoExpectedStatus)
			}

			// --- 1. cold proxy-pull ---
			beforeCold := upstream.requests.Load()
			cold := pull()
			if cold.Code != http.StatusOK {
				return fmt.Sprintf("cold pull of %s/%s:%s (arches=%d) returned %d, want 200", org.Name, image, tag, arches, cold.Code)
			}
			if !bytes.Equal(cold.Body.Bytes(), img.top) {
				return fmt.Sprintf("cold pull of %s/%s:%s served a body that is not the upstream manifest", org.Name, image, tag)
			}
			if upstream.requests.Load() <= beforeCold {
				return fmt.Sprintf("cold pull of %s/%s:%s never contacted the upstream - the round-trip would be vacuous", org.Name, image, tag)
			}
			if msg := requireProxied(image, tag, "after the cold pull"); msg != "" {
				return msg
			}

			pv, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID,
				packages_model.TypeContainer, strings.ToLower(image), strings.ToLower(tag))
			if err != nil {
				return fmt.Sprintf("loading the cached version %s:%s: %v", image, tag, err)
			}

			// --- 2. poison the negative cache for exactly this reference ---
			// Key layout mirrors proxyEnsureManifest: "<upstreamID>|<lower(image)>|<reference>".
			negKey := strconv.FormatInt(up.ID, 10) + "|" + strings.ToLower(image) + "|" + tag
			proxycache.Block(negKey, 900*time.Second)
			if !proxycache.Blocked(negKey) {
				return fmt.Sprintf("failed to poison the negative cache for %s - the delete's unblock would not be exercised", negKey)
			}
			// A sentinel entry for a reference of the SAME image that is not being deleted. It must
			// survive, which is what keeps the "negKey is unblocked" assertion below from passing
			// vacuously: it proves the probe can still report "blocked" after the delete, so an
			// unblocked negKey is a real consequence of the delete and not a blanket cache wipe.
			sentinelKey := strconv.FormatInt(up.ID, 10) + "|" + strings.ToLower(image) + "|" + tag + "-untouched"
			proxycache.Block(sentinelKey, 900*time.Second)

			// --- 3. delete ---
			if err := packages_service.RemoveProxiedVersionAndOrphans(t.Context(), owner, pv); err != nil {
				return fmt.Sprintf("deleting %s:%s: %v", image, tag, err)
			}

			// No cached representation of the version remains: neither the version row...
			if _, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID,
				packages_model.TypeContainer, strings.ToLower(image), strings.ToLower(tag)); err == nil {
				return fmt.Sprintf("version %s:%s still exists after deletion", image, tag)
			}
			// ...nor the container manifest blob the tag resolved to...
			if _, err := container_model.GetContainerBlob(t.Context(), &container_model.BlobSearchOptions{
				OwnerID:    org.ID,
				Image:      strings.ToLower(image),
				IsManifest: true,
				Tag:        strings.ToLower(tag),
				OnlyLead:   true,
			}); err == nil {
				return fmt.Sprintf("the cached manifest of %s:%s is still resolvable after deletion", image, tag)
			}
			// ...nor any per-arch child that only the deleted version referenced.
			for digest := range img.children {
				if _, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID,
					packages_model.TypeContainer, strings.ToLower(image), strings.ToLower(digest)); err == nil {
					return fmt.Sprintf("orphaned per-arch child %s of %s:%s survived the deletion", digest, image, tag)
				}
			}
			// The delete unblocked the poisoned key, so the next pull may reach the upstream.
			if proxycache.Blocked(negKey) {
				return fmt.Sprintf("the negative-cache entry %s is still blocked after the delete - the next pull would be suppressed for the remaining MetadataTTL", negKey)
			}
			if !proxycache.Blocked(sentinelKey) {
				return fmt.Sprintf("the sentinel entry %s was cleared too - the delete must unblock the deleted reference, not the whole negative cache, and without a surviving entry the check above proves nothing", sentinelKey)
			}

			// --- 4. re-pull: a REAL re-fetch, not a warm hit ---
			beforeRepull := upstream.requests.Load()
			repull := pull()
			if repull.Code != http.StatusOK {
				return fmt.Sprintf("re-pull of %s/%s:%s after deletion returned %d, want 200", org.Name, image, tag, repull.Code)
			}
			if !bytes.Equal(repull.Body.Bytes(), img.top) {
				return fmt.Sprintf("re-pull of %s/%s:%s served different bytes than the original pull", org.Name, image, tag)
			}
			if upstream.requests.Load() <= beforeRepull {
				return fmt.Sprintf("re-pull of %s/%s:%s contacted no upstream - the version was resurrected from local state instead of being re-fetched", org.Name, image, tag)
			}
			refetches.Add(1)

			// --- 5. the re-created version is proxied, never internal ---
			if msg := requireProxied(image, tag, "after the re-pull"); msg != "" {
				return msg
			}
			return ""
		},
		genRepullImageBase(),
		genRepullTag(),
		genRepullArchCount(),
	))

	properties.TestingRun(t)

	require.GreaterOrEqual(t, iteration.Load(), int64(params.MinSuccessfulTests),
		"the property must have run at least %d iterations", params.MinSuccessfulTests)
	require.GreaterOrEqual(t, refetches.Load(), int64(params.MinSuccessfulTests),
		"every iteration must have completed a delete followed by a genuine upstream re-fetch")
}
