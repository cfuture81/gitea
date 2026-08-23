// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
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
