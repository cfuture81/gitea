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
	"time"

	"gitea.dev/models/db"
	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/tests"

	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// danglingTargetOwnerID is an owner id no fixture uses, so user_model.GetUserByID must fail for it.
const danglingTargetOwnerID = 999999

// TestPackageUpstreamMissingTargetOwnerGuard covers Requirement 2.5: an enabled upstream whose
// Target_Owner no longer exists must abort the fetch, log an error naming the missing owner, and
// leave the request on the standard local-miss 404 - never cache into a dangling owner.
//
// _Requirements: 2.5_
//
// WHY THIS TEST REACHES INTO THE DATABASE. The condition is structurally unreachable through
// ordinary HTTP. The container proxy resolves upstreams with
// GetEnabledUpstreamsByOwnerAndType(ctx.Package.Owner.ID, ...) and the model normalises every row
// to TargetOwnerID == OwnerID, so a request can only ever resolve to an upstream whose target owner
// IS the live owner the request already resolved to. No sequence of admin CRUD calls plus pulls can
// produce a row that is both reachable and dangling: deleting the owner makes the row unreachable
// (nothing resolves to a deleted owner) rather than dangling. The guard exists precisely for the
// states the HTTP surface cannot construct - a direct database edit, or a future resolver that stops
// scoping by the target owner. So the test writes target_owner_id directly with raw SQL to break the
// TargetOwnerID == OwnerID invariant, which is the only way to exercise
// checkUpstreamTargetOwner's missing-owner branch.
//
// Example-based, not a property: there is exactly one interesting input here (a target owner id that
// does not resolve), and nothing about the behaviour varies with the image or tag.
func TestPackageUpstreamMissingTargetOwnerGuard(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	sha := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
	const mediaType = oci.MediaTypeImageManifest

	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := []byte("target-owner-guard-layer")
	cfgDigest, layerDigest := sha(config), sha(layer)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
		mediaType, cfgDigest, len(config), layerDigest, len(layer)))
	blobs := map[string][]byte{cfgDigest: config, layerDigest: layer}

	// A fake upstream that would happily serve ANY image, so a 404 can only come from the guard
	// refusing to fetch - never from the upstream not having the content.
	var upstreamRequests atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if path, ok := strings.CutPrefix(r.URL.Path, "/v2/"); ok {
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

	// A perfectly valid enabled pull_through container upstream for a LIVE owner.
	up, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
		TargetOwnerID: owner.ID, Type: packages_model.TypeContainer, Name: "dockerhub", URL: ts.URL,
		Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
		MetadataTTL: 900, Enabled: true, Priority: 100, IsAdminManaged: true,
	})
	require.NoError(t, err)
	require.Equal(t, owner.ID, up.OwnerID, "the model must derive OwnerID from TargetOwnerID")

	var tok struct {
		Token string `json:"token"`
	}
	resp := MakeRequest(t, NewRequest(t, "GET", setting.AppURL+"v2/token").AddBasicAuth(owner.Name), http.StatusOK)
	DecodeJSON(t, resp, &tok)
	userToken := "Bearer " + tok.Token

	pull := func(image, reference string) *httptest.ResponseRecorder {
		return MakeRequest(t, NewRequest(t, "GET",
			fmt.Sprintf("%sv2/%s/%s/manifests/%s", setting.AppURL, owner.Name, image, reference)).
			AddTokenAuth(userToken).SetHeader("Accept", mediaType), NoExpectedStatus)
	}

	// Baseline: with the invariant intact the very same setup DOES proxy-fetch. Without this the
	// later 404 could just as well mean "the harness never had a working proxy".
	t.Run("IntactTargetOwnerFetches", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()
		before := upstreamRequests.Load()
		got := pull("guard-sanity", "1.0")
		assert.Equal(t, http.StatusOK, got.Code)
		assert.Equal(t, manifest, got.Body.Bytes())
		assert.Greater(t, upstreamRequests.Load(), before, "the intact upstream must be contacted")
	})

	// Break the TargetOwnerID == OwnerID invariant behind the model's back: the row stays enabled
	// and resolvable for owner 2, but now names a target owner that does not exist.
	_, err = db.GetEngine(t.Context()).Exec("UPDATE package_registry_upstream SET target_owner_id = ? WHERE id = ?", danglingTargetOwnerID, up.ID)
	require.NoError(t, err)

	reloaded, err := packages_model.GetUpstreamByID(t.Context(), up.ID)
	require.NoError(t, err)
	require.EqualValues(t, danglingTargetOwnerID, reloaded.TargetOwnerID, "the dangling target owner must be persisted")
	require.Equal(t, owner.ID, reloaded.OwnerID, "the row must stay resolvable for the live owner, otherwise the guard is never reached")
	require.True(t, reloaded.Enabled)
	_, err = user_model.GetUserByID(t.Context(), danglingTargetOwnerID)
	require.Error(t, err, "owner id %d must not exist", danglingTargetOwnerID)

	t.Run("MissingTargetOwnerAbortsFetch", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()

		logChecker, cleanup := test.NewLogChecker(log.DEFAULT)
		defer cleanup()
		wantLog := fmt.Sprintf("targets owner id %d, which does not exist", danglingTargetOwnerID)
		logChecker.Filter(wantLog).StopMark(wantLog)

		const image, reference = "guard-probe", "1.0"
		before := upstreamRequests.Load()
		got := pull(image, reference)

		// Standard local-miss response, not a 500 and not a silent success.
		assert.Equal(t, http.StatusNotFound, got.Code)
		assert.Contains(t, got.Body.String(), "MANIFEST_UNKNOWN")

		// The fetch was aborted before any network call.
		assert.Equal(t, before, upstreamRequests.Load(), "the upstream must not be contacted when the Target_Owner is missing")

		// An error naming the missing owner was logged.
		filtered, stopped := logChecker.Check(5 * time.Second)
		assert.True(t, stopped, "an error naming the missing Target_Owner must be logged")
		assert.Equal(t, []bool{true}, filtered)

		// Nothing was created - not under the dangling owner, and not under the live one either.
		_, err := packages_model.GetPackageByName(t.Context(), danglingTargetOwnerID, packages_model.TypeContainer, image)
		assert.Error(t, err, "no package row may be created under the dangling owner id %d", danglingTargetOwnerID)
		_, err = packages_model.GetPackageByName(t.Context(), owner.ID, packages_model.TypeContainer, image)
		assert.Error(t, err, "no package row may be created under the request owner either - the fetch was aborted")
		_, err = packages_model.GetVersionByNameAndVersion(t.Context(), danglingTargetOwnerID, packages_model.TypeContainer, image, reference)
		assert.Error(t, err, "no version may be created under the dangling owner")
		_, err = packages_model.GetVersionByNameAndVersion(t.Context(), owner.ID, packages_model.TypeContainer, image, reference)
		assert.Error(t, err, "no version may be created under the request owner")

		// No fetch was accounted for the aborted attempt (the sanity pull's one fetch remains).
		cur, err := packages_model.GetUpstreamByID(t.Context(), up.ID)
		require.NoError(t, err)
		assert.EqualValues(t, 1, cur.FetchCount, "the aborted fetch must not be counted")
	})
}
