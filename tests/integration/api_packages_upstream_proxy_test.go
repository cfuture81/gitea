// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
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
