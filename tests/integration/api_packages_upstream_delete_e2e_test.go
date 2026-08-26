// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/organization"
	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/structs"
	"gitea.dev/modules/test"
	"gitea.dev/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPackageUpstreamDeleteRepullEndToEnd drives the delete -> re-pull round-trip through the real
// HTTP surfaces, complementing Property 8 (which exercises the service function directly) at the
// transport level: it proves each route is actually WIRED to RemoveProxiedVersionAndOrphans, which a
// service-level property cannot show.
//
// _Requirements: 8.2, 8.3, 8.4_
//
// All four container-capable delete paths are covered. The first two are required; the last two are
// the bonus paths, wired in task 5.10 for exactly this consistency:
//
//  1. the OCI route       DELETE /v2/{owner}/{image}/manifests/{reference}
//  2. the web UI          POST   /{owner}/-/packages/container/{name}/{version}
//  3. the admin list      POST   /-/admin/packages/delete?id={versionID}
//  4. the v1 REST API     DELETE /api/v1/packages/{owner}/container/{name}/{version}
//
// Each path gets its own image so the four cycles cannot mask one another, and each cycle asserts
// the same three things the reported bug got wrong: after the delete the version is gone, the next
// pull produces a FRESH upstream fetch (the fake upstream's request counter moves again - a warm
// cache hit or a negative-cache suppression would leave it still), and the re-created version is
// marked proxied rather than rendering as first-party "internal".
func TestPackageUpstreamDeleteRepullEndToEnd(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	admin := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})
	require.True(t, admin.IsAdmin, "user1 must be a site admin for the admin-list delete path")

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
	registryToken := "Bearer " + tok.Token

	ownerSession := loginUser(t, owner.Name)
	adminSession := loginUser(t, admin.Name)
	apiToken := getUserToken(t, owner.Name, auth_model.AccessTokenScopeWritePackage)

	const tag = "1.0"

	// pull performs a container manifest pull of org/image:tag and reports the recorder plus how
	// many requests the fake upstream received while serving it.
	pull := func(t *testing.T, image, mediaType string) (*httptest.ResponseRecorder, int64) {
		t.Helper()
		before := upstream.requests.Load()
		got := MakeRequest(t, NewRequest(t, "GET",
			fmt.Sprintf("%sv2/%s/%s/manifests/%s", setting.AppURL, org.Name, image, tag)).
			AddTokenAuth(registryToken).SetHeader("Accept", mediaType), NoExpectedStatus)
		return got, upstream.requests.Load() - before
	}

	assertProxied := func(t *testing.T, image, phase string) *packages_model.PackageVersion {
		t.Helper()
		pv, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID,
			packages_model.TypeContainer, strings.ToLower(image), tag)
		require.NoError(t, err, "%s: version %s:%s must exist under org %q", phase, image, tag, org.Name)
		cached, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, pv.ID, packages_model.PropertyUpstreamCached)
		require.NoError(t, err)
		require.Len(t, cached, 1, "%s: %s:%s must carry %s - without it the package UI renders it as first-party \"internal\"", phase, image, tag, packages_model.PropertyUpstreamCached)
		assert.Equal(t, "1", cached[0].Value)
		source, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, pv.ID, packages_model.PropertyUpstreamSource)
		require.NoError(t, err)
		require.Len(t, source, 1, "%s: %s:%s must carry %s", phase, image, tag, packages_model.PropertyUpstreamSource)
		assert.Equal(t, up.Name, source[0].Value)
		return pv
	}

	// runCycle: proxy-pull an image cold, delete it through deleteFn, then pull again and prove the
	// second pull was a genuine upstream re-fetch of a version marked proxied.
	runCycle := func(t *testing.T, image string, arches int, deleteFn func(t *testing.T, image string, pv *packages_model.PackageVersion)) {
		t.Helper()
		img := buildColdWarmImage(image, arches)
		upstream.publish(image, tag, img)

		cold, coldRequests := pull(t, image, img.topType)
		require.Equal(t, http.StatusOK, cold.Code, "cold pull of %s:%s", image, tag)
		assert.Equal(t, img.top, cold.Body.Bytes())
		require.Positive(t, coldRequests, "the cold pull must fetch from the fake upstream")
		pv := assertProxied(t, image, "after the cold pull")

		deleteFn(t, image, pv)

		_, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID,
			packages_model.TypeContainer, strings.ToLower(image), tag)
		require.Error(t, err, "version %s:%s must be gone after the delete", image, tag)

		repull, repullRequests := pull(t, image, img.topType)
		require.Equal(t, http.StatusOK, repull.Code, "re-pull of %s:%s after the delete", image, tag)
		assert.Equal(t, img.top, repull.Body.Bytes())
		assert.Positive(t, repullRequests, "the re-pull must be a FRESH upstream fetch, not a warm cache hit")
		assertProxied(t, image, "after the re-pull")
	}

	// --- required: the OCI distribution route ---
	t.Run("OCIRoute", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		runCycle(t, "oci-delete", 0, func(t *testing.T, image string, _ *packages_model.PackageVersion) {
			MakeRequest(t, NewRequest(t, "DELETE",
				fmt.Sprintf("%sv2/%s/%s/manifests/%s", setting.AppURL, org.Name, image, tag)).
				AddTokenAuth(registryToken), http.StatusAccepted)
		})
	})

	// Multi-arch through the same route: the cascade has to take the orphaned per-arch children with
	// the tagged index, otherwise the re-pull would find them cached and fetch less than a full graph.
	t.Run("OCIRouteMultiArch", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		const image = "oci-delete-multi"
		img := buildColdWarmImage(image, 2)
		runCycle(t, image, 2, func(t *testing.T, image string, _ *packages_model.PackageVersion) {
			MakeRequest(t, NewRequest(t, "DELETE",
				fmt.Sprintf("%sv2/%s/%s/manifests/%s", setting.AppURL, org.Name, image, tag)).
				AddTokenAuth(registryToken), http.StatusAccepted)
			for digest := range img.children {
				_, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID,
					packages_model.TypeContainer, image, digest)
				assert.Error(t, err, "orphaned per-arch child %s must be removed with the tagged index", digest)
			}
		})
	})

	// --- required: the web-UI version delete ---
	t.Run("WebUI", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		runCycle(t, "webui-delete", 0, func(t *testing.T, image string, _ *packages_model.PackageVersion) {
			got := ownerSession.MakeRequest(t, NewRequestWithValues(t, "POST",
				fmt.Sprintf("/%s/-/packages/container/%s/%s", org.Name, image, tag), map[string]string{}), NoExpectedStatus)
			require.Equal(t, http.StatusSeeOther, got.Code, "the web-UI delete must redirect after deleting")
		})
	})

	// --- bonus: the site-admin package list ---
	t.Run("AdminList", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		runCycle(t, "admin-delete", 0, func(t *testing.T, image string, pv *packages_model.PackageVersion) {
			adminSession.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/packages/delete", map[string]string{
				"id": strconv.FormatInt(pv.ID, 10),
			}), http.StatusOK) // JSONRedirect
		})
	})

	// --- bonus: the generic v1 REST API ---
	t.Run("APIv1", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		runCycle(t, "apiv1-delete", 0, func(t *testing.T, image string, _ *packages_model.PackageVersion) {
			MakeRequest(t, NewRequest(t, "DELETE",
				fmt.Sprintf("/api/v1/packages/%s/container/%s/%s", org.Name, image, tag)).
				AddTokenAuth(apiToken), http.StatusNoContent)
		})
	})
}
