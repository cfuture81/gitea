// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitea.dev/models/organization"
	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	container_module "gitea.dev/modules/packages/container"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/structs"
	"gitea.dev/modules/test"
	user_service "gitea.dev/services/user"
	"gitea.dev/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPackageUpstreamPublicOrgAnonymousPullAndRename covers the public-org half of the feature:
// the curated orgs exist with public visibility, their packages - hosted AND proxy-cached - are
// pullable without any credentials, and a native Gitea org rename keeps serving all of that content
// under the new org name at the flat path.
//
// _Requirements: 4.1, 4.2, 4.3, 4.4, 7.3_
//
// The curated orgs are created here the same way the operational runbook creates them and the way
// tests/integration/api_packages_upstream_flat_path_prop_test.go does: ordinary Gitea organizations
// via organization.CreateOrganization with structs.VisibleTypePublic. No fork-specific creation
// mechanism exists or is needed (design Q2), which is exactly what the smoke assertion pins down.
//
// The rename is performed by calling Gitea's own rename service, user_service.RenameUser, on
// org.AsUser() - the very function routers/web/org/setting.go's SettingsRenamePost invokes. It is
// called directly rather than through the settings endpoint so the test asserts the CONTENT
// consequence of a rename rather than the org-settings form; that also keeps the container
// repository-property rewrite (services/packages/container.UpdateRepositoryNames, which RenameUser
// calls) inside the code under test.
func TestPackageUpstreamPublicOrgAnonymousPullAndRename(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	admin := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})

	upstream := newColdWarmUpstream()
	ts := httptest.NewServer(upstream)
	defer ts.Close()

	// The curated public orgs of Requirement 4.2, plus the "further orgs follow the same structure"
	// clause of 4.3 - all created through the native path with public visibility.
	orgs := make(map[string]*organization.Organization, len(flatPathOrgNames))
	for _, name := range flatPathOrgNames {
		org := &organization.Organization{
			Name:       name,
			IsActive:   true,
			Type:       user_model.UserTypeOrganization,
			Visibility: structs.VisibleTypePublic,
		}
		require.NoError(t, organization.CreateOrganization(t.Context(), org, owner), "create org %q", name)
		orgs[name] = org
	}

	t.Run("CuratedPublicOrgsExist", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		for _, name := range flatPathOrgNames {
			loaded, err := organization.GetOrgByName(t.Context(), name)
			require.NoError(t, err, "curated org %q must exist", name)
			assert.Equal(t, structs.VisibleTypePublic, loaded.Visibility, "org %q must have public visibility", name)
			assert.True(t, loaded.AsUser().IsOrganization(), "org %q must be an organization", name)
		}
	})

	org := orgs["noenv"]

	up, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		TargetOwnerID: org.ID, Type: packages_model.TypeContainer, Name: "dockerhub", URL: ts.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 100, IsAdminManaged: true,
	})
	require.NoError(t, err)

	var ownerTok, anonTok struct {
		Token string `json:"token"`
	}
	resp := MakeRequest(t, NewRequest(t, "GET", setting.AppURL+"v2/token").AddBasicAuth(owner.Name), http.StatusOK)
	DecodeJSON(t, resp, &ownerTok)
	ownerToken := "Bearer " + ownerTok.Token
	// No credentials at all: the container registry hands out an anonymous bearer token that only
	// grants what a signed-out visitor may see.
	resp = MakeRequest(t, NewRequest(t, "GET", setting.AppURL+"v2/token"), http.StatusOK)
	DecodeJSON(t, resp, &anonTok)
	require.NotEmpty(t, anonTok.Token)
	anonToken := "Bearer " + anonTok.Token

	const (
		hostedImage      = "core-app"
		proxiedImage     = "nats"
		anonColdImage    = "redis"
		tag              = "1.0"
		renamedOrgSuffix = "-renamed"
	)

	hosted := buildColdWarmImage("public-org-hosted", 0)
	proxied := buildColdWarmImage("public-org-proxied", 0)
	anonCold := buildColdWarmImage("public-org-anon-cold", 0)

	upstream.publish(proxiedImage, tag, proxied)
	upstream.publish(anonColdImage, tag, anonCold)

	pushHostedImage(t, ownerToken, org.Name, hostedImage, tag, hosted)

	// pullAs pulls org/image:tag with the given bearer token, reporting the recorder and how many
	// requests reached the fake upstream.
	pullAs := func(t *testing.T, token, orgName, image, mediaType string) (*httptest.ResponseRecorder, int64) {
		t.Helper()
		before := upstream.requests.Load()
		got := MakeRequest(t, NewRequest(t, "GET",
			fmt.Sprintf("%sv2/%s/%s/manifests/%s", setting.AppURL, orgName, image, tag)).
			AddTokenAuth(token).SetHeader("Accept", mediaType), NoExpectedStatus)
		return got, upstream.requests.Load() - before
	}

	// Cache the proxied image with the owner's credentials, so the anonymous test below is about
	// anonymous READ of the org's (now cached) package rather than about proxying.
	warm, warmRequests := pullAs(t, ownerToken, org.Name, proxiedImage, proxied.topType)
	require.Equal(t, http.StatusOK, warm.Code)
	require.Positive(t, warmRequests)

	t.Run("AnonymousPullHostedImage", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		got, requests := pullAs(t, anonToken, org.Name, hostedImage, hosted.topType)
		require.Equal(t, http.StatusOK, got.Code, "an anonymous visitor must be able to pull a hosted image from a public org")
		assert.Equal(t, hosted.top, got.Body.Bytes())
		assert.Zero(t, requests, "a hosted image is served locally")
	})

	t.Run("AnonymousPullProxiedImage", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		got, requests := pullAs(t, anonToken, org.Name, proxiedImage, proxied.topType)
		require.Equal(t, http.StatusOK, got.Code, "an anonymous visitor must be able to pull a proxy-cached image from a public org")
		assert.Equal(t, proxied.top, got.Body.Bytes())
		assert.Zero(t, requests, "the cached copy is served locally")
	})

	t.Run("AnonymousColdProxyPull", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		// An anonymous cache miss still fetches and caches: the proxy stores under
		// ctx.Package.Owner (the org), so it needs no doer identity.
		got, requests := pullAs(t, anonToken, org.Name, anonColdImage, anonCold.topType)
		require.Equal(t, http.StatusOK, got.Code)
		assert.Equal(t, anonCold.top, got.Body.Bytes())
		assert.Positive(t, requests, "an anonymous cache miss must still reach the upstream")

		pv, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID, packages_model.TypeContainer, anonColdImage, tag)
		require.NoError(t, err, "the anonymously fetched image must be cached under the org")
		cached, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, pv.ID, packages_model.PropertyUpstreamCached)
		require.NoError(t, err)
		require.Len(t, cached, 1)
	})

	t.Run("OrgRenameKeepsServingContent", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()

		oldName := org.Name
		newName := oldName + renamedOrgSuffix
		require.NoError(t, user_service.RenameUser(t.Context(), org.AsUser(), newName, admin))

		renamed, err := organization.GetOrgByName(t.Context(), newName)
		require.NoError(t, err, "the org must exist under its new name")
		require.Equal(t, org.ID, renamed.ID, "a rename must keep the same org id, so the upstream row stays attached")

		// Existing hosted content is still served, at the flat path under the NEW org name.
		got, requests := pullAs(t, ownerToken, newName, hostedImage, hosted.topType)
		require.Equal(t, http.StatusOK, got.Code, "hosted content must still be served after the rename")
		assert.Equal(t, hosted.top, got.Body.Bytes())
		assert.Zero(t, requests, "hosted content stays local after the rename")

		// And so is previously cached proxied content.
		got, requests = pullAs(t, ownerToken, newName, proxiedImage, proxied.topType)
		require.Equal(t, http.StatusOK, got.Code, "cached proxied content must still be served after the rename")
		assert.Equal(t, proxied.top, got.Body.Bytes())
		assert.Zero(t, requests, "cached content stays local after the rename")

		// Anonymous pull keeps working under the new name too (the org is still public).
		got, _ = pullAs(t, anonToken, newName, hostedImage, hosted.topType)
		assert.Equal(t, http.StatusOK, got.Code, "anonymous pull must keep working under the new org name")

		// The container repository property was rewritten to the new flat "<org>/<image>".
		for _, image := range []string{hostedImage, proxiedImage} {
			p, err := packages_model.GetPackageByName(t.Context(), org.ID, packages_model.TypeContainer, image)
			require.NoError(t, err)
			props, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypePackage, p.ID, container_module.PropertyRepository)
			require.NoError(t, err)
			require.Len(t, props, 1, "package %q must carry exactly one repository property", image)
			assert.Equal(t, newName+"/"+image, props[0].Value, "the repository property must follow the rename")
		}

		// The proxy still resolves for the renamed org: a fresh cache miss is fetched and cached.
		const afterRenameImage = "busybox"
		afterRename := buildColdWarmImage("public-org-after-rename", 0)
		upstream.publish(afterRenameImage, tag, afterRename)
		got, requests = pullAs(t, ownerToken, newName, afterRenameImage, afterRename.topType)
		require.Equal(t, http.StatusOK, got.Code)
		assert.Equal(t, afterRename.top, got.Body.Bytes())
		assert.Positive(t, requests, "the upstream must still be reachable for the renamed org")

		cur, err := packages_model.GetUpstreamByID(t.Context(), up.ID)
		require.NoError(t, err)
		assert.Equal(t, org.ID, cur.OwnerID, "the upstream stays scoped to the org id, not its name")
	})
}
