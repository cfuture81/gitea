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
	"sync/atomic"
	"testing"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/routers"
	"gitea.dev/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Authorization and feature-flag behavior of the site-admin registry-upstream surface.
//
// # Why there are two routers here
//
// `setting.Packages.EnableUpstreamProxy` is read at *route-registration* time for the admin
// surface: `routers/web/web.go` wraps the `/-/admin/packages/upstreams` group in
// `if setting.Packages.EnableUpstreamProxy`, and the `EnableUpstreamProxy` template value the
// admin navbar reads comes from the `ctxDataSet` on the `/-/admin` group, which is also evaluated
// once when the group is built. The integration harness builds its router exactly once in
// `testMain` (`testWebRoutes = routers.NormalRoutes()`), and `tests/sqlite.ini.tmpl` does not set
// `ENABLE_UPSTREAM_PROXY`, so the shared harness router is a **flag-off** router.
//
// Consequences, which shape every test below:
//
//   - `test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, ...)` alone can NOT add or
//     remove admin routes, and can NOT change the navbar's `EnableUpstreamProxy` value. Asserting
//     route absence against the shared router while merely flipping the setting would prove
//     nothing about the flag — the routes are absent regardless.
//   - Therefore the flag-on side is exercised by building a *second* router with the flag set
//     (`buildUpstreamProxyRouter`) and swapping it in for the duration of a subtest. That second
//     router is the positive control that makes the flag-off 404s meaningful: the same URL that
//     404s on the flag-off router returns 200 on the flag-on router.
//   - Request-time gates are a different story and need no router swap: the container proxy hook
//     (`getEnabledContainerUpstreams`) and the package-UI source badge
//     (`ctx.Data["EnableUpstreamProxy"]`) both read the setting per request, so mocking the
//     variable does flip them.
//
// # What is deliberately NOT claimed
//
// At the HTTP layer a non-registered route and the handlers' own defensive
// `requireUpstreamProxy` guard are indistinguishable: both answer `404` with the byte-identical
// body `"Not found.\n"`. `web.Router` keeps its chi mux unexported, so a test outside the package
// cannot walk the route tree either. The flag-off assertions below therefore claim exactly what
// Requirements 1.7 / 9.2 ask for — the surface is *not reachable* with the flag off and *is*
// reachable with it on — and do not claim to prove which of the two mechanisms produced the 404.

// buildUpstreamProxyRouter builds a second, independent route tree with
// setting.Packages.EnableUpstreamProxy forced on, and installs it as the router MakeRequest and
// TestSession.MakeRequest serve against. The returned func restores the harness router.
//
// This is safe to call from a test because NormalRoutes() constructs a fresh web.Router and the
// integration config leaves the globally-registering options off (metrics disabled, captcha
// disabled), so no process-wide registration is repeated. It does mutate the package-level
// testWebRoutes, so a caller must not run in parallel with other integration tests.
func buildUpstreamProxyRouter(t *testing.T) func() {
	t.Helper()
	restoreSetting := test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)
	router := routers.NormalRoutes()
	restoreRouter := test.MockVariableValue(&testWebRoutes, router)
	return func() {
		restoreRouter()
		restoreSetting()
	}
}

// TestAdminPackagesUpstreamsAuthorization exercises the admin upstream routes on a flag-ON route
// tree: a site admin reaches them, a signed-in non-admin is denied, and an anonymous request is
// redirected to the login page — i.e. the standard Gitea admin-area denial, because the routes
// live inside the `/-/admin` group and inherit its `adminReq` middleware (Requirement 1.2).
//
// The site-admin 200s are also the positive control for TestAdminPackagesUpstreamsFlagOff: they
// prove these exact URLs resolve when the flag is on, so the 404s asserted there are attributable
// to the flag and not to a mistyped path.
func TestAdminPackagesUpstreamsAuthorization(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer buildUpstreamProxyRouter(t)()

	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	// an existing row so the {id} routes address something real rather than 404ing on lookup
	up, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		OwnerID: user.ID, TargetOwnerID: user.ID, Type: packages_model.TypeContainer,
		Name: "dockerhub", URL: "http://upstream.invalid",
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 100, IsAdminManaged: true,
	})
	require.NoError(t, err)

	const list = "/-/admin/packages/upstreams"
	editPath := fmt.Sprintf("%s/%d", list, up.ID)
	deletePath := fmt.Sprintf("%s/%d/delete", list, up.ID)

	t.Run("SiteAdminAllowed", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		session := loginUser(t, "user1")
		resp := session.MakeRequest(t, NewRequest(t, "GET", list), http.StatusOK)
		// the page really is the upstream page, not some other admin page that happens to 200
		assert.Contains(t, resp.Body.String(), "dockerhub")
	})

	t.Run("NavbarEntryPresentWhenFlagOn", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		// Positive control for FlagOff/NavbarEntryAbsent below: with the flag on, the admin
		// navbar on a *different* admin page links to the upstream page.
		session := loginUser(t, "user1")
		resp := session.MakeRequest(t, NewRequest(t, "GET", "/-/admin/packages"), http.StatusOK)
		assert.Contains(t, resp.Body.String(), "/-/admin/packages/upstreams")
	})

	t.Run("NonAdminForbidden", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		session := loginUser(t, "user2")
		session.MakeRequest(t, NewRequest(t, "GET", list), http.StatusForbidden)
		session.MakeRequest(t, NewRequestWithValues(t, "POST", list, map[string]string{
			"type": "container", "name": "evil", "url": "http://evil.invalid",
			"target_owner_id": fmt.Sprintf("%d", user.ID),
		}), http.StatusForbidden)
		session.MakeRequest(t, NewRequestWithValues(t, "POST", editPath, map[string]string{
			"name": "renamed-by-non-admin",
		}), http.StatusForbidden)
		session.MakeRequest(t, NewRequest(t, "POST", deletePath), http.StatusForbidden)
	})

	t.Run("AnonymousRedirectedToLogin", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		for _, tc := range []struct{ method, path string }{
			{"GET", list},
			{"POST", list},
			{"POST", editPath},
			{"POST", deletePath},
		} {
			resp := MakeRequest(t, NewRequest(t, tc.method, tc.path), http.StatusSeeOther)
			assert.Contains(t, resp.Header().Get("Location"), "/user/login",
				"%s %s must redirect an anonymous caller to the login page", tc.method, tc.path)
		}
	})

	t.Run("DeniedRequestsPersistedNothing", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		// The denied create/edit/delete above must have had no effect on the configuration.
		all, err := packages_model.GetAllUpstreams(t.Context())
		require.NoError(t, err)
		require.Len(t, all, 1, "no upstream may be created or removed by a denied request")
		assert.Equal(t, up.ID, all[0].ID)
		assert.Equal(t, "dockerhub", all[0].Name, "a denied edit must not rename the upstream")
	})
}

// TestAdminPackagesUpstreamsFlagOff asserts that with ENABLE_UPSTREAM_PROXY off the fork is
// indistinguishable from vanilla Gitea across every surface this feature touches
// (Requirements 1.7, 9.2).
//
// The admin-route and navbar assertions run against the harness router, which is built flag-off
// (see the file comment); their positive controls live in
// TestAdminPackagesUpstreamsAuthorization. The badge and container-proxy assertions read the flag
// per request and therefore carry their own in-test flag-on controls.
func TestAdminPackagesUpstreamsFlagOff(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	require.False(t, setting.Packages.EnableUpstreamProxy,
		"the integration harness router must be built with the proxy flag off for this test to mean anything")

	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	const list = "/-/admin/packages/upstreams"

	t.Run("AdminRoutesNotReachable", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		session := loginUser(t, "user1")
		// Control: the admin packages page this group sits next to does exist for user1, so a
		// 404 below is about the upstream surface, not about the path prefix or the session.
		// The complementary control is TestAdminPackagesUpstreamsAuthorization/SiteAdminAllowed,
		// where the very same URL returns 200 on a flag-on route tree.
		session.MakeRequest(t, NewRequest(t, "GET", "/-/admin/packages"), http.StatusOK)

		session.MakeRequest(t, NewRequest(t, "GET", list), http.StatusNotFound)
		session.MakeRequest(t, NewRequestWithValues(t, "POST", list, map[string]string{
			"type": "container", "name": "dockerhub", "url": "http://upstream.invalid",
			"target_owner_id": fmt.Sprintf("%d", user.ID),
		}), http.StatusNotFound)
		session.MakeRequest(t, NewRequestWithValues(t, "POST", list+"/1", map[string]string{
			"name": "renamed",
		}), http.StatusNotFound)
		session.MakeRequest(t, NewRequest(t, "POST", list+"/1/delete"), http.StatusNotFound)

		// nothing was created by the POST that 404'd
		all, err := packages_model.GetAllUpstreams(t.Context())
		require.NoError(t, err)
		assert.Empty(t, all)
	})

	t.Run("NavbarEntryAbsent", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		session := loginUser(t, "user1")
		resp := session.MakeRequest(t, NewRequest(t, "GET", "/-/admin/packages"), http.StatusOK)
		body := resp.Body.String()
		// Control: the unconditional sibling entry from the same navbar block ("Assets" ->
		// repositories) is present, so the assertion below is about the conditional upstream entry
		// and not about a navbar that failed to render at all.
		assert.Contains(t, body, "/-/admin/repos")
		assert.NotContains(t, body, "/-/admin/packages/upstreams")
	})

	t.Run("PerOwnerRouteRemoved", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		// The per-owner surface is deleted outright, not flag-gated, so this holds unconditionally
		// and needs no flag-on counterpart.
		session := loginUser(t, "user2")
		resp := session.MakeRequest(t, NewRequest(t, "GET", "/user/settings/packages"), http.StatusOK)
		assert.NotContains(t, resp.Body.String(), "/user/settings/packages/upstreams",
			"the package settings page must not link to the removed upstreams page")
		// Control: a sibling subpath of the same route group still resolves, so the 404 below is
		// about the removed "upstreams" child and not about the group rejecting subpaths at all.
		session.MakeRequest(t, NewRequest(t, "GET", "/user/settings/packages/rules/add"), http.StatusOK)
		session.MakeRequest(t, NewRequest(t, "GET", "/user/settings/packages/upstreams"), http.StatusNotFound)
	})

	t.Run("NoSourceBadgeInPackageUI", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()

		// a plain first-party package, so the badge would render as "internal" if it rendered
		req := NewRequestWithBody(t, "PUT", "/api/packages/user2/generic/badge-probe/1.0.0/file.bin",
			bytes.NewReader([]byte{1, 2, 3})).AddBasicAuth(user.Name)
		MakeRequest(t, req, http.StatusCreated)

		// needles unique to templates/package/shared/source_badge.tmpl
		const internalNeedle = "First-party image built and published internally"
		const proxiedNeedle = "Cached from an upstream registry"

		pages := []string{
			"/user2/-/packages",
			"/user2/-/packages/generic/badge-probe/1.0.0",
		}

		for _, page := range pages {
			resp := MakeRequest(t, NewRequest(t, "GET", page).AddBasicAuth(user.Name), http.StatusOK)
			body := resp.Body.String()
			assert.NotContains(t, body, internalNeedle, "%s must render no source badge with the flag off", page)
			assert.NotContains(t, body, proxiedNeedle, "%s must render no source badge with the flag off", page)
		}

		// Control: the badge is gated on ctx.Data["EnableUpstreamProxy"], which is set per request,
		// so flipping the setting must make the same pages render it. Without this the NotContains
		// assertions above could pass for any unrelated reason.
		t.Run("BadgeRendersWhenFlagOn", func(t *testing.T) {
			defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()
			for _, page := range pages {
				resp := MakeRequest(t, NewRequest(t, "GET", page).AddBasicAuth(user.Name), http.StatusOK)
				assert.Contains(t, resp.Body.String(), internalNeedle,
					"%s must render the source badge with the flag on", page)
			}
		})
	})

	t.Run("ContainerCacheMissIsVanilla404", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()

		sha := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
		configJSON := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
		layer := []byte("fake-layer-bytes")
		cfgDigest, layerDigest := sha(configJSON), sha(layer)
		const mediaType = "application/vnd.oci.image.manifest.v1+json"
		manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
			mediaType, cfgDigest, len(configJSON), layerDigest, len(layer)))

		// in-process fake upstream; the hit counter is what proves the proxy stayed inert
		var hits atomic.Int64
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v2/flagoff/manifests/1.0":
				hits.Add(1)
				w.Header().Set("Content-Type", mediaType)
				_, _ = w.Write(manifest)
			case "/v2/flagoff/blobs/" + cfgDigest:
				_, _ = w.Write(configJSON)
			case "/v2/flagoff/blobs/" + layerDigest:
				_, _ = w.Write(layer)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer ts.Close()

		_, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
			OwnerID: user.ID, TargetOwnerID: user.ID, Type: packages_model.TypeContainer,
			Name: "dockerhub", URL: ts.URL,
			Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
			MetadataTTL: 900, Enabled: true, Priority: 100, IsAdminManaged: true,
		})
		require.NoError(t, err)

		var tok struct {
			Token string `json:"token"`
		}
		resp := MakeRequest(t, NewRequest(t, "GET", setting.AppURL+"v2/token").AddBasicAuth(user.Name), http.StatusOK)
		DecodeJSON(t, resp, &tok)
		userToken := "Bearer " + tok.Token
		manifestURL := setting.AppURL + "v2/user2/flagoff/manifests/1.0"

		newPull := func() *RequestWrapper {
			return NewRequest(t, "GET", manifestURL).AddTokenAuth(userToken).SetHeader("Accept", mediaType)
		}

		// flag off: standard local-miss 404, and the configured upstream is never contacted
		MakeRequest(t, newPull(), http.StatusNotFound)
		assert.EqualValues(t, 0, hits.Load(), "the upstream must not be contacted with the flag off")
		_, err = packages_model.GetVersionByNameAndVersion(t.Context(), user.ID, packages_model.TypeContainer, "flagoff", "1.0")
		assert.Error(t, err, "no version may be cached with the flag off")

		// Control: the container proxy reads the flag per request, so with it on the very same
		// pull succeeds and does contact the upstream. Without this, the 404 above would be
		// consistent with a broken fake upstream or a wrong URL.
		t.Run("SamePullSucceedsWhenFlagOn", func(t *testing.T) {
			defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()
			resp := MakeRequest(t, newPull(), http.StatusOK)
			assert.Equal(t, manifest, resp.Body.Bytes())
			assert.Positive(t, hits.Load(), "the upstream must be contacted with the flag on")
		})
	})
}
