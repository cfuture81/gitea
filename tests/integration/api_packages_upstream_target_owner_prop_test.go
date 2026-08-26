// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	packages_model "gitea.dev/models/packages"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/tests"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// targetOwnerFixtures are the fixture owners used as generated Target_Owners. Deliberately a mix
// of users (1, 2, 4, 5) and organizations (3, 6, 7, 17) so the property is exercised across owner
// kinds rather than pinned to a single owner.
var targetOwnerFixtures = []string{"user1", "user2", "org3", "user4", "user5", "org6", "org7", "org17"}

// genTargetOwnerName yields one of the fixture owner names (users and organizations).
func genTargetOwnerName() gopter.Gen {
	return gen.OneConstOf(
		targetOwnerFixtures[0], targetOwnerFixtures[1], targetOwnerFixtures[2], targetOwnerFixtures[3],
		targetOwnerFixtures[4], targetOwnerFixtures[5], targetOwnerFixtures[6], targetOwnerFixtures[7],
	)
}

// genProxiedImageBase yields realistic single-segment container image names. It stays
// single-segment on purpose: multi-segment/flat-path resolution is Property 3's subject, this
// property is about WHICH owner the cached package lands under.
func genProxiedImageBase() gopter.Gen {
	return gen.OneConstOf("nats", "mongo", "redis", "alpine", "busybox", "core-app", "proxy.lab", "hello_world")
}

// genProxiedTag yields tags accepted by the container reference pattern.
func genProxiedTag() gopter.Gen {
	return gen.OneConstOf("latest", "1.0", "v2.1.3", "stable", "3", "edge", "2026.07.26")
}

// Feature: proxy-registry-admin-and-orgs, Property 2: Cached packages land under the Target_Owner
//
// For any enabled `pull_through` upstream whose Target_Owner is `O` and any image available from a
// fake upstream, a cache-miss pull SHALL create the resulting cached package under owner `O`.
//
// **Validates: Requirements 2.3**
//
// The model keeps the invariant TargetOwnerID == OwnerID (normalizeTargetOwner) and the container
// proxy resolves upstreams by ctx.Package.Owner.ID, storing under ctx.Package.Owner - so "cached
// under the Target_Owner" is structurally the case today. This property pins that invariant across
// varying owners (users and orgs) so a future refactor of the resolver or of the storage owner
// cannot silently relocate cached content. Each upstream is inserted with ONLY TargetOwnerID set,
// mirroring the admin panel's Target_Owner selection and letting normalizeTargetOwner derive the
// scoping OwnerID from it.
//
// Note on task 4.3: a divergent Target_Owner (checkUpstreamTargetOwner) correctly aborts the fetch
// and yields a 404 with nothing cached. That is a separate, example-tested behavior; the generators
// here stay on the valid (target owner == request owner) case this property is about.
//
// Hermetic: the upstream is an in-process httptest server, no external network. One test
// environment, one fake upstream and one bearer token are shared across all iterations; each
// iteration uses a UNIQUE image name so it is always a genuine cache miss (a repeated name would
// hit the warm local cache and make the property vacuous). Non-vacuousness is additionally
// asserted directly: the fake upstream's request counter must strictly increase per iteration.
func TestPropertyUpstreamCachedUnderTargetOwner(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	sha := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

	const mediaType = "application/vnd.oci.image.manifest.v1+json"

	// One tiny manifest graph (config + single layer) reused for every generated image, so the
	// per-iteration cost stays dominated by the local store, not by fake payload size.
	configJSON := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := []byte("fake-layer")
	cfgDigest, layerDigest := sha(configJSON), sha(layer)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
		mediaType, cfgDigest, len(configJSON), layerDigest, len(layer)))
	blobs := map[string][]byte{cfgDigest: configJSON, layerDigest: layer}

	// Fake registry: serves the same manifest for ANY /v2/<image>/manifests/<ref> and the two
	// referenced blobs. upstreamHits proves the upstream really was contacted per iteration.
	var upstreamHits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		path, ok := strings.CutPrefix(r.URL.Path, "/v2/")
		if ok {
			if _, _, found := strings.Cut(path, "/manifests/"); found {
				w.Header().Set("Content-Type", mediaType)
				_, _ = w.Write(manifest)
				return
			}
			if _, dgst, found := strings.Cut(path, "/blobs/"); found {
				if body, ok := blobs[dgst]; ok {
					_, _ = w.Write(body)
					return
				}
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// Configure one enabled pull_through upstream per candidate Target_Owner. Only TargetOwnerID
	// is set - InsertUpstream's normalizeTargetOwner derives OwnerID from it.
	owners := make(map[string]*user_model.User, len(targetOwnerFixtures))
	for _, name := range targetOwnerFixtures {
		owner, err := user_model.GetUserByName(t.Context(), name)
		require.NoError(t, err)
		owners[name] = owner

		_, err = packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
			TargetOwnerID: owner.ID, Type: packages_model.TypeContainer, Name: "dockerhub", URL: ts.URL,
			Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
			MetadataTTL: 900, Enabled: true, Priority: 100, IsAdminManaged: true,
		})
		require.NoError(t, err)
	}

	// Site-admin container bearer token: read access to every owner's namespace, acquired once.
	var tok struct {
		Token string `json:"token"`
	}
	resp := MakeRequest(t, NewRequest(t, "GET", setting.AppURL+"v2/token").AddBasicAuth("user1"), http.StatusOK)
	DecodeJSON(t, resp, &tok)
	userToken := "Bearer " + tok.Token

	// iteration uniquifies the generated image name so every iteration is a cold cache miss.
	var iteration atomic.Int64

	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100
	params.MaxSize = 16 // generated inputs are small enumerations; keep the HTTP work bounded

	properties := gopter.NewProperties(params)

	properties.Property("a cache-miss pull caches the image under the upstream's Target_Owner", prop.ForAll(
		func(ownerName, imageBase, tag string) string {
			owner := owners[ownerName]
			image := fmt.Sprintf("%s-i%d", imageBase, iteration.Add(1))

			hitsBefore := upstreamHits.Load()

			req := NewRequest(t, "GET", fmt.Sprintf("%sv2/%s/%s/manifests/%s", setting.AppURL, ownerName, image, tag)).
				AddTokenAuth(userToken).SetHeader("Accept", mediaType)
			pull := MakeRequest(t, req, NoExpectedStatus)
			if pull.Code != http.StatusOK {
				return fmt.Sprintf("pull of %s/%s:%s returned %d, want 200", ownerName, image, tag, pull.Code)
			}

			// Non-vacuousness: this iteration really was a cache miss served by the fake upstream.
			if upstreamHits.Load() <= hitsBefore {
				return fmt.Sprintf("upstream was not contacted for %s/%s:%s - iteration would be vacuous", ownerName, image, tag)
			}

			// The cached package exists under the Target_Owner...
			if _, err := packages_model.GetVersionByNameAndVersion(t.Context(), owner.ID,
				packages_model.TypeContainer, strings.ToLower(image), strings.ToLower(tag)); err != nil {
				return fmt.Sprintf("cached version %s:%s missing under Target_Owner %s (id %d): %v", image, tag, ownerName, owner.ID, err)
			}

			// ...and under no other owner.
			for otherName, other := range owners {
				if other.ID == owner.ID {
					continue
				}
				if _, err := packages_model.GetVersionByNameAndVersion(t.Context(), other.ID,
					packages_model.TypeContainer, strings.ToLower(image), strings.ToLower(tag)); err == nil {
					return fmt.Sprintf("cached version %s:%s must not exist under owner %s (id %d), only under Target_Owner %s", image, tag, otherName, other.ID, ownerName)
				}
			}
			return ""
		},
		genTargetOwnerName(),
		genProxiedImageBase(),
		genProxiedTag(),
	))

	properties.TestingRun(t)
}
