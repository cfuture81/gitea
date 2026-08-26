// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gitea.dev/models/organization"
	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/structs"
	"gitea.dev/modules/test"
	"gitea.dev/tests"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

const (
	coldWarmManifestMediaType = oci.MediaTypeImageManifest
	coldWarmIndexMediaType    = oci.MediaTypeImageIndex
)

// coldWarmArches are the platform names used for the per-arch children of a generated multi-arch
// index, consumed in order (a 2-arch index uses amd64 + arm64).
var coldWarmArches = []string{"amd64", "arm64", "s390x"}

// genColdWarmImageBase yields realistic single-segment container image names. A per-iteration
// suffix is appended by the property body so every iteration is a genuine cold miss.
func genColdWarmImageBase() gopter.Gen {
	return gen.OneConstOf("nats", "mongo", "redis", "alpine", "busybox", "core-app", "wallet-sync")
}

// genColdWarmTag yields references within the container reference pattern, lowercase so the
// generated tag and the stored (lowercased) version are the same string.
func genColdWarmTag() gopter.Gen {
	return gen.OneConstOf("latest", "main", "stable", "1.0", "v2.1.3", "20260101")
}

// genColdWarmArchCount decides the SHAPE of the generated manifest graph: 0 means a plain
// single-arch image manifest, 1..3 means an OCI index with that many per-arch children. The design
// requires this property to cover single-arch AND multi-arch graphs, because a multi-arch pull
// recurses through fetchAndStoreManifest and stores several versions - a warm hit that re-fetched
// only the children would still look "served" at the top level.
func genColdWarmArchCount() gopter.Gen {
	return gen.IntRange(0, 3)
}

func coldWarmSHA(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// coldWarmImage is a generated manifest graph: either a single image manifest, or an index with
// per-arch children. Every body embeds the per-iteration nonce, so all digests are unique to the
// iteration and no iteration can be satisfied by content another one already stored.
type coldWarmImage struct {
	top      []byte            // the body the tag resolves to (manifest or index)
	topType  string            // its media type
	isIndex  bool              // whether top is an index
	children map[string][]byte // child manifest digest -> body (empty for single-arch)
	blobs    map[string][]byte // blob digest -> body
}

func buildColdWarmImage(nonce string, arches int) *coldWarmImage {
	img := &coldWarmImage{
		children: map[string][]byte{},
		blobs:    map[string][]byte{},
	}

	// buildArch produces one config blob, one layer blob and the image manifest referencing them.
	buildArch := func(arch string) []byte {
		config := []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux","rootfs":{"type":"layers","diff_ids":[]},"nonce":%q}`, arch, nonce))
		layer := []byte("cold-warm-layer-" + arch + "-" + nonce)
		cfgDigest, layerDigest := coldWarmSHA(config), coldWarmSHA(layer)
		img.blobs[cfgDigest] = config
		img.blobs[layerDigest] = layer
		return []byte(fmt.Sprintf(
			`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
			coldWarmManifestMediaType, cfgDigest, len(config), layerDigest, len(layer)))
	}

	if arches == 0 {
		img.top = buildArch("amd64")
		img.topType = coldWarmManifestMediaType
		return img
	}

	descriptors := make([]string, 0, arches)
	for i := range arches {
		arch := coldWarmArches[i%len(coldWarmArches)]
		manifest := buildArch(arch)
		digest := coldWarmSHA(manifest)
		img.children[digest] = manifest
		descriptors = append(descriptors, fmt.Sprintf(
			`{"mediaType":%q,"digest":%q,"size":%d,"platform":{"architecture":%q,"os":"linux"}}`,
			coldWarmManifestMediaType, digest, len(manifest), arch))
	}
	img.top = []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[%s]}`,
		coldWarmIndexMediaType, strings.Join(descriptors, ",")))
	img.topType = coldWarmIndexMediaType
	img.isIndex = true
	return img
}

// coldWarmUpstream is an in-process fake OCI registry that serves only what an iteration has
// explicitly published and counts EVERY request it receives. That counter is the assertion this
// property rests on: it must increase while the cold pull fetches, and must not move by a single
// request across the warm pull.
type coldWarmUpstream struct {
	mu       sync.Mutex
	bodies   map[string][]byte
	types    map[string]string
	requests atomic.Int64
}

func newColdWarmUpstream() *coldWarmUpstream {
	return &coldWarmUpstream{bodies: map[string][]byte{}, types: map[string]string{}}
}

// publish makes img reachable at the upstream address of image:tag. RemotePrefix is empty on the
// configured upstream, so the upstream address equals the image name.
func (f *coldWarmUpstream) publish(image, tag string, img *coldWarmImage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := "/v2/" + image
	f.bodies[base+"/manifests/"+tag] = img.top
	f.types[base+"/manifests/"+tag] = img.topType
	for digest, body := range img.children {
		f.bodies[base+"/manifests/"+digest] = body
		f.types[base+"/manifests/"+digest] = coldWarmManifestMediaType
	}
	for digest, body := range img.blobs {
		f.bodies[base+"/blobs/"+digest] = body
	}
}

func (f *coldWarmUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	f.mu.Lock()
	body, ok := f.bodies[r.URL.Path]
	ct := f.types[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	_, _ = w.Write(body)
}

// Feature: proxy-registry-admin-and-orgs, Property 6: Cold miss fetches and caches; warm hit serves locally
//
// For any image absent locally but available from an enabled `pull_through` upstream, the first pull
// SHALL fetch the image, cache it under the org, and serve it, and a subsequent pull for the same
// image SHALL be served from local cache without an additional upstream fetch.
//
// **Validates: Requirements 6.3**
//
// The generated input includes the SHAPE of the manifest graph, not just its names: a plain
// single-arch image manifest and an OCI index with one to three per-arch children are all
// exercised, because the multi-arch case recurses through fetchAndStoreManifest and stores several
// versions - a warm pull that silently re-fetched the children while serving the index locally
// would still look correct from the response alone.
//
// The assertion that carries the weight is the fake upstream's request counter:
//
//   - the cold pull must move it (otherwise the iteration proved nothing - the image would have
//     been served from an earlier iteration's cache and the property would be vacuous), and
//   - the warm pull must not move it by a single request, blob fetches included.
//
// Byte identity of both responses is asserted as an independent witness, so "warm served nothing
// useful" cannot pass as "warm served locally". Each iteration uses a UNIQUE image name and
// nonce-derived digests, so the cold half is always genuinely cold.
//
// Hermetic: the upstream is an in-process httptest server; no external network is used. One test
// environment, one fake upstream and one bearer token are shared across all iterations.
func TestPropertyUpstreamColdMissThenWarmHit(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	upstream := newColdWarmUpstream()
	ts := httptest.NewServer(upstream)
	defer ts.Close()

	// A public org (the Nexus-equivalent pull target) with one enabled pull_through container
	// upstream whose Target_Owner is the org itself.
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

	var iteration atomic.Int64
	var coldFetches atomic.Int64

	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100

	properties := gopter.NewProperties(params)

	properties.Property("a cold pull fetches and caches, and the following warm pull contacts no upstream", prop.ForAll(
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

			// --- cold: absent locally, available upstream ---
			beforeCold := upstream.requests.Load()
			cold := pull()
			if cold.Code != http.StatusOK {
				return fmt.Sprintf("cold pull of %s/%s:%s (arches=%d) returned %d, want 200", org.Name, image, tag, arches, cold.Code)
			}
			if !bytes.Equal(cold.Body.Bytes(), img.top) {
				return fmt.Sprintf("cold pull of %s/%s:%s served a body that is not the upstream manifest", org.Name, image, tag)
			}
			fetched := upstream.requests.Load() - beforeCold
			if fetched == 0 {
				return fmt.Sprintf("cold pull of %s/%s:%s never contacted the upstream - it was not a cache miss, so the iteration is vacuous", org.Name, image, tag)
			}
			coldFetches.Add(1)

			// It was cached UNDER THE ORG, with the flat name, and marked as proxy content.
			pv, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID,
				packages_model.TypeContainer, strings.ToLower(image), strings.ToLower(tag))
			if err != nil {
				return fmt.Sprintf("after the cold pull no version %s:%s exists under org %q: %v", image, tag, org.Name, err)
			}
			cached, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, pv.ID, packages_model.PropertyUpstreamCached)
			if err != nil {
				return fmt.Sprintf("reading %s of %s:%s: %v", packages_model.PropertyUpstreamCached, image, tag, err)
			}
			if len(cached) != 1 || cached[0].Value != "1" {
				return fmt.Sprintf("cached version %s:%s carries %d %s properties, want exactly one with value \"1\"", image, tag, len(cached), packages_model.PropertyUpstreamCached)
			}
			// The whole graph was cached, not only the tagged top: every per-arch child of a
			// multi-arch index is stored under its digest.
			for digest := range img.children {
				if _, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID,
					packages_model.TypeContainer, strings.ToLower(image), strings.ToLower(digest)); err != nil {
					return fmt.Sprintf("per-arch child %s of %s:%s was not cached: %v", digest, image, tag, err)
				}
			}

			// --- warm: the same image, now present locally ---
			beforeWarm := upstream.requests.Load()
			warm := pull()
			if warm.Code != http.StatusOK {
				return fmt.Sprintf("warm pull of %s/%s:%s returned %d, want 200", org.Name, image, tag, warm.Code)
			}
			if !bytes.Equal(warm.Body.Bytes(), img.top) {
				return fmt.Sprintf("warm pull of %s/%s:%s served different bytes than the cold pull", org.Name, image, tag)
			}
			if extra := upstream.requests.Load() - beforeWarm; extra != 0 {
				return fmt.Sprintf("warm pull of %s/%s:%s (arches=%d) sent %d request(s) to the upstream; a cached image must be served locally", org.Name, image, tag, arches, extra)
			}

			// The proxy accounted exactly one fetch for the cold pull and a hit for the warm one.
			cur, err := packages_model.GetUpstreamByID(t.Context(), up.ID)
			if err != nil {
				return fmt.Sprintf("reload upstream: %v", err)
			}
			if cur.HitCount == 0 {
				return fmt.Sprintf("upstream recorded no cache hit after the warm pull of %s:%s", image, tag)
			}
			return ""
		},
		genColdWarmImageBase(),
		genColdWarmTag(),
		genColdWarmArchCount(),
	))

	properties.TestingRun(t)

	require.GreaterOrEqual(t, iteration.Load(), int64(params.MinSuccessfulTests),
		"the property must have run at least %d iterations", params.MinSuccessfulTests)
	require.GreaterOrEqual(t, coldFetches.Load(), int64(params.MinSuccessfulTests),
		"every iteration must have performed a real cold fetch from the fake upstream")
}
