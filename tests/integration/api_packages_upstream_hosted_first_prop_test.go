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
	"strings"
	"sync/atomic"
	"testing"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/tests"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

const hostedFirstMediaType = "application/vnd.oci.image.manifest.v1+json"

// sha256Digest returns the OCI digest string of b.
func sha256Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// genHostedImageName yields lowercase container image names within Gitea's image-name pattern
// (`\A[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*\z`), including separator-joined and
// multi-segment names. Lengths are deliberately short: every iteration performs a real upload and
// pull, so generated size is bounded to keep 100 iterations fast.
// The pattern is anchored so gopter's SuchThat sieve rejects off-pattern candidates during
// shrinking too - an unanchored pattern would match a substring and could report a counterexample
// that is not a legal image name.
func genHostedImageName() gopter.Gen {
	return gen.RegexMatch(`^[a-z][a-z0-9]{0,6}([._-][a-z0-9]{1,4})?(/[a-z][a-z0-9]{0,6})?$`)
}

// genHostedTag yields references within Gitea's reference pattern
// (`\A[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}\z`), restricted to lowercase so the generated tag and the
// stored (lowercased) version are the same string and the assertions stay about resolution order
// rather than case normalization.
func genHostedTag() gopter.Gen {
	return gen.RegexMatch(`^[a-z0-9_][a-z0-9._-]{0,8}$`)
}

// Feature: proxy-registry-admin-and-orgs, Property 5: Hosted content shadows proxied content
//
// For any image that exists locally under an org (a Hosted_Image or previously-cached content), a
// pull for that image SHALL be served from local storage without contacting any upstream, even when
// a same-named upstream is configured.
//
// **Validates: Requirements 6.2, 6.5**
//
// This is the executable half of the resolution invariant documented on
// `getManifestFromContextOrProxy` and in FORK-MAINTENANCE.md §3a: local (hosted or
// previously-cached) first; on a local miss only, enabled upstreams in `priority ASC, id ASC`
// order; first match wins. If a rebase ever moved the upstream loop ahead of the local lookup, a
// public Docker Hub image could silently shadow a first-party image of the same name — a
// supply-chain-shaped failure that no "the pull works" test would catch. Two independent witnesses
// are asserted per iteration:
//
//  1. the fake upstream's request counter stays at 0 (it was never contacted), and
//  2. the bytes served are the hosted manifest and are NOT the fake upstream's manifest,
//
// so a regression is caught both by contact and by content. The fake upstream deliberately answers
// EVERY image name and tag with a valid but different manifest, so an inverted order would succeed
// and shadow the hosted image rather than 404 and fall back to the correct answer.
func TestPropertyHostedContentShadowsProxiedContent(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	// user2 is a member of the owner team of org3, a public fixture organization, so it may push
	// hosted packages into the org namespace.
	pusher := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	org := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 3})

	// The fake upstream serves ONE manifest for every image name and tag it is asked about, with
	// config/layer bytes that no hosted image in this test can produce. That makes "served hosted"
	// distinguishable from "served upstream" by content as well as by the request counter.
	upstreamConfig := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"provenance":"UPSTREAM-FAKE-DOCKER-HUB"}`)
	upstreamLayer := []byte("UPSTREAM-FAKE-DOCKER-HUB-layer-bytes")
	upstreamCfgDigest, upstreamLayerDigest := sha256Digest(upstreamConfig), sha256Digest(upstreamLayer)
	upstreamManifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
		hostedFirstMediaType, upstreamCfgDigest, len(upstreamConfig), upstreamLayerDigest, len(upstreamLayer)))

	// Counts EVERY request the upstream receives, whatever the path - the property asserts this
	// stays 0 for the whole run.
	var upstreamRequests atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", hostedFirstMediaType)
			_, _ = w.Write(upstreamManifest)
		case strings.HasSuffix(r.URL.Path, "/blobs/"+upstreamCfgDigest):
			_, _ = w.Write(upstreamConfig)
		case strings.HasSuffix(r.URL.Path, "/blobs/"+upstreamLayerDigest):
			_, _ = w.Write(upstreamLayer)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	// A same-named upstream is configured for the org for the whole run: enabled, pull_through,
	// and able to serve any image the property generates.
	up, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		OwnerID: org.ID, TargetOwnerID: org.ID, Type: packages_model.TypeContainer,
		Name: "dockerhub", URL: ts.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 100,
	})
	require.NoError(t, err)

	var tok struct {
		Token string `json:"token"`
	}
	resp := MakeRequest(t, NewRequest(t, "GET", setting.AppURL+"v2/token").AddBasicAuth(pusher.Name), http.StatusOK)
	DecodeJSON(t, resp, &tok)
	token := "Bearer " + tok.Token

	// Each iteration needs a genuinely fresh hosted image, so the generated name is suffixed with
	// an iteration counter. The generator still decides the shape of the name; the suffix only
	// guarantees the local lookup starts from a miss for every iteration.
	var iteration atomic.Int64

	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100

	properties := gopter.NewProperties(params)

	properties.Property("a hosted image is served locally and the configured upstream is never contacted", prop.ForAll(
		func(rawImage, tag string) string {
			image := fmt.Sprintf("%s-%d", rawImage, iteration.Add(1))
			base := fmt.Sprintf("%sv2/%s/%s", setting.AppURL, org.Name, image)

			// Hosted content: unique per iteration and never equal to the upstream's bytes.
			config := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"provenance":"HOSTED-FIRST-PARTY","image":%q,"tag":%q}`, image, tag))
			layer := []byte("hosted-first-party-layer-" + image + ":" + tag)
			manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
				hostedFirstMediaType, sha256Digest(config), len(config), sha256Digest(layer), len(layer)))

			// HOST the image locally - directly through the registry's own upload path, never
			// through the proxy.
			for _, blob := range [][]byte{config, layer} {
				req := NewRequestWithBody(t, "POST", base+"/blobs/uploads?digest="+sha256Digest(blob), bytes.NewReader(blob)).
					AddTokenAuth(token)
				if got := MakeRequest(t, req, NoExpectedStatus); got.Code != http.StatusCreated {
					return fmt.Sprintf("hosted blob upload for %s:%s returned %d, want 201", image, tag, got.Code)
				}
			}
			req := NewRequestWithBody(t, "PUT", base+"/manifests/"+tag, bytes.NewReader(manifest)).
				AddTokenAuth(token).SetHeader("Content-Type", hostedFirstMediaType)
			if got := MakeRequest(t, req, NoExpectedStatus); got.Code != http.StatusCreated {
				return fmt.Sprintf("hosted manifest upload for %s:%s returned %d, want 201", image, tag, got.Code)
			}

			// PULL it. A same-named upstream is enabled and would happily serve this image.
			req = NewRequest(t, "GET", base+"/manifests/"+tag).
				AddTokenAuth(token).SetHeader("Accept", hostedFirstMediaType)
			got := MakeRequest(t, req, NoExpectedStatus)
			if got.Code != http.StatusOK {
				return fmt.Sprintf("pull of hosted %s:%s returned %d, want 200", image, tag, got.Code)
			}
			if bytes.Equal(got.Body.Bytes(), upstreamManifest) {
				return fmt.Sprintf("pull of hosted %s:%s served the UPSTREAM manifest - proxied content shadowed hosted content", image, tag)
			}
			if !bytes.Equal(got.Body.Bytes(), manifest) {
				return fmt.Sprintf("pull of hosted %s:%s served neither the hosted nor the upstream manifest: %q", image, tag, got.Body.String())
			}

			// The same invariant holds for blobs (getBlobFromContextOrProxy).
			req = NewRequest(t, "GET", base+"/blobs/"+sha256Digest(layer)).AddTokenAuth(token)
			got = MakeRequest(t, req, NoExpectedStatus)
			if got.Code != http.StatusOK {
				return fmt.Sprintf("pull of hosted layer of %s:%s returned %d, want 200", image, tag, got.Code)
			}
			if !bytes.Equal(got.Body.Bytes(), layer) {
				return fmt.Sprintf("pull of hosted layer of %s:%s served %q, want the hosted layer", image, tag, got.Body.String())
			}

			// Witness 1: the upstream was never contacted, for this or any earlier iteration.
			if n := upstreamRequests.Load(); n != 0 {
				return fmt.Sprintf("the upstream was contacted %d time(s) while serving hosted content (at %s:%s)", n, image, tag)
			}

			// The upstream row proves the proxy path really was live for this request - it was
			// resolved and counted a cache hit - while never recording a fetch. Without this the
			// property could pass vacuously if no upstream applied to the org at all.
			cur, err := packages_model.GetUpstreamByID(t.Context(), up.ID)
			if err != nil {
				return fmt.Sprintf("reload upstream: %v", err)
			}
			if cur.FetchCount != 0 {
				return fmt.Sprintf("upstream recorded %d fetch(es) while serving hosted content (at %s:%s)", cur.FetchCount, image, tag)
			}
			if cur.HitCount == 0 {
				return fmt.Sprintf("upstream recorded no cache hit at %s:%s - the configured upstream was not part of the resolution, so the property would pass vacuously", image, tag)
			}
			return ""
		},
		genHostedImageName(),
		genHostedTag(),
	))

	properties.TestingRun(t)
}
