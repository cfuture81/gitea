// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPackageUpstreamProxyMaven exercises the pull-through + freeze proxy and ordered group
// resolution against hermetic fake upstreams (no real network).
func TestPackageUpstreamProxyMaven(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	const pomPath = "/com/example/demo/1.0.0/demo-1.0.0.pom"
	const pomBody = `<project><modelVersion>4.0.0</modelVersion></project>`
	base := "/api/packages/user2/maven"

	// fake upstream serving exactly one artifact; counts hits to prove when it is (not) contacted.
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pomPath {
			hits.Add(1)
			_, _ = w.Write([]byte(pomBody))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	up, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		OwnerID: user.ID, Type: packages_model.TypeMaven, Name: "central", URL: ts.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 100,
	})
	require.NoError(t, err)

	reload := func(t *testing.T) *packages_model.PackageRegistryUpstream {
		u, err := packages_model.GetUpstreamByID(t.Context(), up.ID)
		require.NoError(t, err)
		return u
	}

	t.Run("PullThroughColdFetch", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		req := NewRequest(t, "GET", base+pomPath).AddBasicAuth(user.Name)
		resp := MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, pomBody, resp.Body.String())
		assert.EqualValues(t, 1, hits.Load(), "upstream contacted once on cold miss")
		assert.EqualValues(t, 1, reload(t).FetchCount)
	})

	t.Run("WarmServedFromCache", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		before := hits.Load()
		req := NewRequest(t, "GET", base+pomPath).AddBasicAuth(user.Name)
		resp := MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, pomBody, resp.Body.String())
		assert.Equal(t, before, hits.Load(), "warm hit must not contact upstream")
		assert.EqualValues(t, 1, reload(t).FetchCount)
		assert.Positive(t, reload(t).HitCount)
	})

	t.Run("FrozenServesCacheOnly", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		u := reload(t)
		u.Mode = packages_model.UpstreamModeFrozen
		require.NoError(t, packages_model.UpdateUpstream(t.Context(), u))

		before := hits.Load()
		// uncached artifact -> denied, upstream never contacted
		req := NewRequest(t, "GET", base+"/com/example/demo/2.0.0/demo-2.0.0.pom").AddBasicAuth(user.Name)
		MakeRequest(t, req, http.StatusNotFound)
		assert.Equal(t, before, hits.Load(), "frozen must not contact upstream")
		assert.EqualValues(t, 1, reload(t).FetchCount, "fetch count unchanged while frozen")

		// cached artifact still served
		req = NewRequest(t, "GET", base+pomPath).AddBasicAuth(user.Name)
		resp := MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, pomBody, resp.Body.String())
	})
}

// TestPackageUpstreamProxyMavenGroup verifies ordered multi-upstream resolution (hosted-first is
// inherent; proxies are tried by priority, first hit wins).
func TestPackageUpstreamProxyMavenGroup(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	const pomPath = "/org/only/second/3.0.0/second-3.0.0.pom"
	const pomBody = `<project><modelVersion>4.0.0</modelVersion></project>`
	base := "/api/packages/user2/maven"

	var firstHits, secondHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		w.WriteHeader(http.StatusNotFound) // first upstream never has the artifact
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pomPath {
			secondHits.Add(1)
			_, _ = w.Write([]byte(pomBody))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer second.Close()

	up1, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		OwnerID: user.ID, Type: packages_model.TypeMaven, Name: "first", URL: first.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 10,
	})
	require.NoError(t, err)
	up2, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		OwnerID: user.ID, Type: packages_model.TypeMaven, Name: "second", URL: second.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 20,
	})
	require.NoError(t, err)

	t.Run("FallthroughByPriority", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		req := NewRequest(t, "GET", base+pomPath).AddBasicAuth(user.Name)
		resp := MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, pomBody, resp.Body.String())
		assert.Positive(t, firstHits.Load(), "first (priority 10) is tried and misses")
		assert.EqualValues(t, 1, secondHits.Load(), "second (priority 20) serves it")

		u1, err := packages_model.GetUpstreamByID(t.Context(), up1.ID)
		require.NoError(t, err)
		u2, err := packages_model.GetUpstreamByID(t.Context(), up2.ID)
		require.NoError(t, err)
		assert.EqualValues(t, 0, u1.FetchCount, "missing upstream records no fetch")
		assert.EqualValues(t, 1, u2.FetchCount, "serving upstream records the fetch")
	})
}

// buildNpmTarball builds a minimal valid npm .tgz (package/package.json with name+version).
func buildNpmTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(zw)
	pkgJSON := []byte(`{"name":"` + name + `","version":"` + version + `"}`)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(pkgJSON))}))
	_, err := tw.Write(pkgJSON)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())
	return gz.Bytes()
}

// TestPackageUpstreamProxyNpm exercises the npm packument-proxy + lazy tarball caching + freeze
// against a hermetic fake npm registry.
func TestPackageUpstreamProxyNpm(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()
	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	tgz := buildNpmTarball(t, "demo", "1.0.0")
	var tarballHits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/demo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"demo","dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":{"name":"demo","version":"1.0.0","dist":{"tarball":"http://upstream.invalid/demo/-/demo-1.0.0.tgz"}}}}`))
		case "/demo/-/demo-1.0.0.tgz":
			tarballHits.Add(1)
			_, _ = w.Write(tgz)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	up, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		OwnerID: user.ID, Type: packages_model.TypeNpm, Name: "npmjs", URL: ts.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 100,
	})
	require.NoError(t, err)
	base := "/api/packages/user2/npm"
	tarball := base + "/demo/-/1.0.0/demo-1.0.0.tgz"

	t.Run("PackumentProxyRewritesTarball", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		req := NewRequest(t, "GET", base+"/demo").AddBasicAuth(user.Name)
		resp := MakeRequest(t, req, http.StatusOK)
		assert.Contains(t, resp.Body.String(), "/api/packages/user2/npm/demo/-/1.0.0/demo-1.0.0.tgz")
	})
	t.Run("ColdTarballFetch", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		req := NewRequest(t, "GET", tarball).AddBasicAuth(user.Name)
		resp := MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, tgz, resp.Body.Bytes())
		assert.EqualValues(t, 1, tarballHits.Load())
		u, _ := packages_model.GetUpstreamByID(t.Context(), up.ID)
		assert.EqualValues(t, 1, u.FetchCount)
	})
	t.Run("WarmServedFromCache", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		before := tarballHits.Load()
		req := NewRequest(t, "GET", tarball).AddBasicAuth(user.Name)
		MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, before, tarballHits.Load(), "warm hit must not contact upstream")
	})
	t.Run("FrozenServesCacheOnly", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		u, _ := packages_model.GetUpstreamByID(t.Context(), up.ID)
		u.Mode = packages_model.UpstreamModeFrozen
		require.NoError(t, packages_model.UpdateUpstream(t.Context(), u))
		before := tarballHits.Load()
		// uncached version denied, upstream not contacted
		req := NewRequest(t, "GET", base+"/demo/-/2.0.0/demo-2.0.0.tgz").AddBasicAuth(user.Name)
		MakeRequest(t, req, http.StatusNotFound)
		assert.Equal(t, before, tarballHits.Load())
		// cached tarball still served
		req = NewRequest(t, "GET", tarball).AddBasicAuth(user.Name)
		MakeRequest(t, req, http.StatusOK)
	})
}

// TestPackageUpstreamProxyPyPI exercises the PyPI simple-index proxy + lazy file caching + freeze
// against a hermetic fake index/file server.
func TestPackageUpstreamProxyPyPI(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()
	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	fileBody := []byte("fake-sdist-content")
	var fileHits atomic.Int64
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/demo/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><a href="` + ts.URL + `/files/demo-1.0.0.tar.gz#sha256=abc">demo-1.0.0.tar.gz</a></body></html>`))
		case "/files/demo-1.0.0.tar.gz":
			fileHits.Add(1)
			_, _ = w.Write(fileBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	up, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		OwnerID: user.ID, Type: packages_model.TypePyPI, Name: "pypi", URL: ts.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 100,
	})
	require.NoError(t, err)

	enc := base64.RawURLEncoding.EncodeToString([]byte(ts.URL + "/files/demo-1.0.0.tar.gz"))
	base := "/api/packages/user2/pypi"
	fileURL := base + "/files/demo/1.0.0/demo-1.0.0.tar.gz?u=" + enc

	t.Run("SimpleIndexProxyRewritesLinks", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		req := NewRequest(t, "GET", base+"/simple/demo").AddBasicAuth(user.Name)
		resp := MakeRequest(t, req, http.StatusOK)
		assert.Contains(t, resp.Body.String(), "/api/packages/user2/pypi/files/demo/1.0.0/demo-1.0.0.tar.gz?u=")
	})
	t.Run("ColdFileFetch", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		req := NewRequest(t, "GET", fileURL).AddBasicAuth(user.Name)
		resp := MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, fileBody, resp.Body.Bytes())
		assert.EqualValues(t, 1, fileHits.Load())
		u, _ := packages_model.GetUpstreamByID(t.Context(), up.ID)
		assert.EqualValues(t, 1, u.FetchCount)
	})
	t.Run("WarmServedFromCache", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		before := fileHits.Load()
		req := NewRequest(t, "GET", base+"/files/demo/1.0.0/demo-1.0.0.tar.gz").AddBasicAuth(user.Name)
		MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, before, fileHits.Load(), "warm hit must not contact upstream")
	})
	t.Run("FrozenServesCacheOnly", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		u, _ := packages_model.GetUpstreamByID(t.Context(), up.ID)
		u.Mode = packages_model.UpstreamModeFrozen
		require.NoError(t, packages_model.UpdateUpstream(t.Context(), u))
		before := fileHits.Load()
		// uncached file (with a u param) must still be denied while frozen, upstream not contacted
		enc2 := base64.RawURLEncoding.EncodeToString([]byte(ts.URL + "/files/demo-2.0.0.tar.gz"))
		req := NewRequest(t, "GET", base+"/files/demo/2.0.0/demo-2.0.0.tar.gz?u="+enc2).AddBasicAuth(user.Name)
		MakeRequest(t, req, http.StatusNotFound)
		assert.Equal(t, before, fileHits.Load())
		// cached file still served
		req = NewRequest(t, "GET", base+"/files/demo/1.0.0/demo-1.0.0.tar.gz").AddBasicAuth(user.Name)
		MakeRequest(t, req, http.StatusOK)
	})
}

// TestPackageUpstreamProxyContainer exercises the Docker/OCI pull-through + freeze proxy against a
// hermetic fake registry (incl. the Bearer token-challenge flow, manifest + blob fetch).
func TestPackageUpstreamProxyContainer(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()
	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	sha := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
	configJSON := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := []byte("fake-layer-bytes")
	cfgDigest, layerDigest := sha(configJSON), sha(layer)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"%s","size":%d}]}`,
		cfgDigest, len(configJSON), layerDigest, len(layer)))
	const mediaType = "application/vnd.oci.image.manifest.v1+json"

	var manifestHits atomic.Int64
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// token endpoint (no auth required)
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"faketoken"}`))
			return
		}
		// Bearer challenge for /v2 resources
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+ts.URL+`/token",service="fake",scope="repository:demo:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/demo/manifests/1.0":
			manifestHits.Add(1)
			w.Header().Set("Content-Type", mediaType)
			_, _ = w.Write(manifest)
		case "/v2/demo/blobs/" + cfgDigest:
			_, _ = w.Write(configJSON)
		case "/v2/demo/blobs/" + layerDigest:
			_, _ = w.Write(layer)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	up, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		OwnerID: user.ID, Type: packages_model.TypeContainer, Name: "dockerhub", URL: ts.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 100,
	})
	require.NoError(t, err)

	// Gitea container-registry bearer token for user2.
	var tok struct {
		Token string `json:"token"`
	}
	resp := MakeRequest(t, NewRequest(t, "GET", setting.AppURL+"v2/token").AddBasicAuth(user.Name), http.StatusOK)
	DecodeJSON(t, resp, &tok)
	userToken := "Bearer " + tok.Token
	url := setting.AppURL + "v2/user2/demo"

	t.Run("PullThroughColdManifest", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		req := NewRequest(t, "GET", url+"/manifests/1.0").AddTokenAuth(userToken).
			SetHeader("Accept", mediaType)
		resp := MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, manifest, resp.Body.Bytes())
		assert.Positive(t, manifestHits.Load())
		u, _ := packages_model.GetUpstreamByID(t.Context(), up.ID)
		assert.EqualValues(t, 1, u.FetchCount)
	})
	t.Run("CachedBlobServed", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		req := NewRequest(t, "GET", url+"/blobs/"+layerDigest).AddTokenAuth(userToken)
		resp := MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, layer, resp.Body.Bytes())
	})
	t.Run("WarmManifestFromCache", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		before := manifestHits.Load()
		req := NewRequest(t, "GET", url+"/manifests/1.0").AddTokenAuth(userToken).SetHeader("Accept", mediaType)
		MakeRequest(t, req, http.StatusOK)
		assert.Equal(t, before, manifestHits.Load(), "warm manifest must not contact upstream")
	})
	t.Run("FrozenServesCacheOnly", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		u, _ := packages_model.GetUpstreamByID(t.Context(), up.ID)
		u.Mode = packages_model.UpstreamModeFrozen
		require.NoError(t, packages_model.UpdateUpstream(t.Context(), u))
		before := manifestHits.Load()
		// uncached tag denied, upstream not contacted
		req := NewRequest(t, "GET", url+"/manifests/2.0").AddTokenAuth(userToken).SetHeader("Accept", mediaType)
		MakeRequest(t, req, http.StatusNotFound)
		assert.Equal(t, before, manifestHits.Load())
		// cached tag still served
		req = NewRequest(t, "GET", url+"/manifests/1.0").AddTokenAuth(userToken).SetHeader("Accept", mediaType)
		MakeRequest(t, req, http.StatusOK)
	})
}

// TestPackageUpstreamProxyContainerMultiArchCached pulls a multi-arch image (an OCI index with two
// per-arch sub-manifests) through the proxy and asserts that EVERY version the pull creates - the
// tagged index and both per-arch children stored under their digest - carries the
// upstream.cached / upstream.source properties. Without that, residual/re-pulled sub-manifests
// render as first-party "internal" content in the package UI.
func TestPackageUpstreamProxyContainerMultiArchCached(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()
	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	sha := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

	const manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	const indexMediaType = "application/vnd.oci.image.index.v1+json"

	// one config+layer blob pair and one image manifest per architecture
	type archImage struct {
		arch     string
		config   []byte
		layer    []byte
		manifest []byte
	}
	arches := []*archImage{{arch: "amd64"}, {arch: "arm64"}}
	blobs := map[string][]byte{}
	children := map[string][]byte{} // child manifest digest -> manifest body
	for _, ai := range arches {
		ai.config = []byte(`{"architecture":"` + ai.arch + `","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
		ai.layer = []byte("fake-layer-bytes-" + ai.arch)
		ai.manifest = []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
			manifestMediaType, sha(ai.config), len(ai.config), sha(ai.layer), len(ai.layer)))
		blobs[sha(ai.config)] = ai.config
		blobs[sha(ai.layer)] = ai.layer
		children[sha(ai.manifest)] = ai.manifest
	}
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"architecture":"amd64","os":"linux"}},{"mediaType":%q,"digest":%q,"size":%d,"platform":{"architecture":"arm64","os":"linux"}}]}`,
		indexMediaType,
		manifestMediaType, sha(arches[0].manifest), len(arches[0].manifest),
		manifestMediaType, sha(arches[1].manifest), len(arches[1].manifest)))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ref, ok := strings.CutPrefix(r.URL.Path, "/v2/multi/manifests/"); ok {
			if ref == "1.0" {
				w.Header().Set("Content-Type", indexMediaType)
				_, _ = w.Write(index)
				return
			}
			if body, ok := children[ref]; ok {
				w.Header().Set("Content-Type", manifestMediaType)
				_, _ = w.Write(body)
				return
			}
		}
		if dgst, ok := strings.CutPrefix(r.URL.Path, "/v2/multi/blobs/"); ok {
			if body, ok := blobs[dgst]; ok {
				_, _ = w.Write(body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	_, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		OwnerID: user.ID, Type: packages_model.TypeContainer, Name: "dockerhub", URL: ts.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 100,
	})
	require.NoError(t, err)

	var tok struct {
		Token string `json:"token"`
	}
	resp := MakeRequest(t, NewRequest(t, "GET", setting.AppURL+"v2/token").AddBasicAuth(user.Name), http.StatusOK)
	DecodeJSON(t, resp, &tok)
	userToken := "Bearer " + tok.Token

	req := NewRequest(t, "GET", setting.AppURL+"v2/user2/multi/manifests/1.0").
		AddTokenAuth(userToken).SetHeader("Accept", indexMediaType)
	resp = MakeRequest(t, req, http.StatusOK)
	assert.Equal(t, index, resp.Body.Bytes())

	// the tagged index AND every per-arch child must be marked as proxied cache content
	refs := []string{"1.0"}
	for d := range children {
		refs = append(refs, d)
	}
	for _, ref := range refs {
		t.Run("Cached_"+ref, func(t *testing.T) {
			pv, err := packages_model.GetVersionByNameAndVersion(t.Context(), user.ID, packages_model.TypeContainer, "multi", strings.ToLower(ref))
			require.NoError(t, err, "version %q must exist after the multi-arch pull", ref)

			cached, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, pv.ID, packages_model.PropertyUpstreamCached)
			require.NoError(t, err)
			require.Len(t, cached, 1, "version %q must carry exactly one %s property", ref, packages_model.PropertyUpstreamCached)
			assert.Equal(t, "1", cached[0].Value)

			source, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, pv.ID, packages_model.PropertyUpstreamSource)
			require.NoError(t, err)
			require.Len(t, source, 1, "version %q must carry exactly one %s property", ref, packages_model.PropertyUpstreamSource)
			assert.Equal(t, "dockerhub", source[0].Value)
		})
	}
}
