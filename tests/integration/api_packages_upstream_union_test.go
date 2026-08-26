// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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

// pushHostedImage uploads img's blobs and manifest into org/image:tag through the registry's own
// upload path, i.e. as first-party hosted content that never went through the proxy.
func pushHostedImage(t *testing.T, token, org, image, tag string, img *coldWarmImage) {
	t.Helper()
	base := fmt.Sprintf("%sv2/%s/%s", setting.AppURL, org, image)
	for digest, body := range img.blobs {
		MakeRequest(t, NewRequestWithBody(t, "POST", base+"/blobs/uploads?digest="+digest, bytes.NewReader(body)).
			AddTokenAuth(token), http.StatusCreated)
	}
	MakeRequest(t, NewRequestWithBody(t, "PUT", base+"/manifests/"+tag, bytes.NewReader(img.top)).
		AddTokenAuth(token).SetHeader("Content-Type", img.topType), http.StatusCreated)
}

// TestPackageUpstreamHostedProxiedUnion covers the Nexus `docker-group` equivalence: one public org
// endpoint serving first-party hosted images and Docker-Hub-proxied images at the same time, each
// resolved correctly and each labelled for what it is.
//
// _Requirements: 6.1, 6.2, 6.3_
//
// The two halves are distinguished by the fake upstream's request counter, which is the only
// observable that separates "served locally" from "fetched": the hosted pull must not move it at
// all (R6.2), the proxied pull must (R6.3). Both images live under the same org at the same time
// (R6.1), and the fake upstream deliberately also HAS the hosted image's name published, so a
// hosted pull that leaked into the proxy path would succeed with the wrong bytes rather than fail -
// the counter and the byte comparison catch it either way.
func TestPackageUpstreamHostedProxiedUnion(t *testing.T) {
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
	registryToken := "Bearer " + tok.Token

	const (
		hostedImage  = "core-app" // first-party, imported by skopeo in production
		proxiedImage = "nats"     // public, pulled through from Docker Hub
		tag          = "1.0"
	)

	hosted := buildColdWarmImage("hosted-core-app", 0)
	proxied := buildColdWarmImage("proxied-nats", 0)

	// The upstream can serve BOTH names. The hosted one must never be asked for.
	upstream.publish(hostedImage, tag, buildColdWarmImage("upstream-shadow-attempt", 0))
	upstream.publish(proxiedImage, tag, proxied)

	pushHostedImage(t, registryToken, org.Name, hostedImage, tag, hosted)

	pull := func(t *testing.T, image, mediaType string) (*httptest.ResponseRecorder, int64) {
		t.Helper()
		before := upstream.requests.Load()
		got := MakeRequest(t, NewRequest(t, "GET",
			fmt.Sprintf("%sv2/%s/%s/manifests/%s", setting.AppURL, org.Name, image, tag)).
			AddTokenAuth(registryToken).SetHeader("Accept", mediaType), NoExpectedStatus)
		return got, upstream.requests.Load() - before
	}

	t.Run("HostedServedWithoutUpstream", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		got, requests := pull(t, hostedImage, hosted.topType)
		require.Equal(t, http.StatusOK, got.Code)
		assert.Equal(t, hosted.top, got.Body.Bytes(), "the hosted manifest must be served, not the upstream's")
		assert.Zero(t, requests, "a hosted image must be served locally - the upstream must not be contacted")
	})

	t.Run("ProxiedFetchedFromUpstream", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		got, requests := pull(t, proxiedImage, proxied.topType)
		require.Equal(t, http.StatusOK, got.Code)
		assert.Equal(t, proxied.top, got.Body.Bytes())
		assert.Positive(t, requests, "an image absent locally must be fetched from the configured upstream")
	})

	t.Run("BothCoexistUnderOneOrg", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()

		ps, err := packages_model.GetPackagesByType(t.Context(), org.ID, packages_model.TypeContainer)
		require.NoError(t, err)
		names := make([]string, 0, len(ps))
		for _, p := range ps {
			names = append(names, p.LowerName)
		}
		assert.ElementsMatch(t, []string{hostedImage, proxiedImage}, names,
			"one org must hold the hosted and the proxied image simultaneously")

		// And they are labelled for what they are: the source badge renders "proxied" when
		// upstream.cached is present and "internal" when it is absent.
		hostedVersion, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID, packages_model.TypeContainer, hostedImage, tag)
		require.NoError(t, err)
		hostedCached, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, hostedVersion.ID, packages_model.PropertyUpstreamCached)
		require.NoError(t, err)
		assert.Empty(t, hostedCached, "a hosted image must not be marked as upstream cache content")

		proxiedVersion, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID, packages_model.TypeContainer, proxiedImage, tag)
		require.NoError(t, err)
		proxiedCached, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, proxiedVersion.ID, packages_model.PropertyUpstreamCached)
		require.NoError(t, err)
		require.Len(t, proxiedCached, 1, "a proxied image must be marked as upstream cache content")
		proxiedSource, err := packages_model.GetPropertiesByName(t.Context(), packages_model.PropertyTypeVersion, proxiedVersion.ID, packages_model.PropertyUpstreamSource)
		require.NoError(t, err)
		require.Len(t, proxiedSource, 1)
		assert.Equal(t, up.Name, proxiedSource[0].Value)
	})

	t.Run("HostedStaysLocalAfterAProxiedFetch", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		// Re-assert the hosted half AFTER the upstream has been used, so an upstream client or
		// token that became "warm" cannot start intercepting hosted pulls unnoticed.
		got, requests := pull(t, hostedImage, hosted.topType)
		require.Equal(t, http.StatusOK, got.Code)
		assert.Equal(t, hosted.top, got.Body.Bytes())
		assert.Zero(t, requests, "the hosted image must still be served locally once the upstream is in use")
	})
}
