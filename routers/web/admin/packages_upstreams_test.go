// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package admin

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
	"gitea.dev/services/contexttest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Example-based tests for the site-admin upstream CRUD surface: the four handler happy paths
// (create / list / edit / delete) and the three rejections that must persist nothing (missing
// Target_Owner, non-existent Target_Owner, invalid pinned-tags JSON).
//
// The fixture owners used below are the site admin user1 (id 1, the doer), the individual user2
// (id 2) and the organization org3 (id 3). `package_registry_upstream` has no fixture file, so
// every test starts from an empty upstream table (unittest deletes fixture-less tables of
// registered models on PrepareTestEnv).
//
// Flash assertions match on the i18n *key* because contexttest installs translation.MockLocale,
// whose Tr renders "<key>" / "<key>:<arg>" rather than the en-US text.

const (
	fixtureOwnerUser2 int64 = 2
	fixtureOwnerOrg3  int64 = 3
	// missingOwnerID is far above every fixture user id, so it can never resolve to an owner.
	missingOwnerID int64 = 999999
)

// enableUpstreamProxy turns the feature flag on for the duration of one test. The flag is a
// process-global, so the previous value is restored on cleanup.
func enableUpstreamProxy(t *testing.T) {
	t.Helper()
	prev := setting.Packages.EnableUpstreamProxy
	setting.Packages.EnableUpstreamProxy = true
	t.Cleanup(func() { setting.Packages.EnableUpstreamProxy = prev })
}

// mockAdminContext builds a mock web context for reqPath with the fixture site admin as the doer
// and form (when given) submitted as POST body values.
func mockAdminContext(t *testing.T, reqPath string, form url.Values) (*context.Context, *httptest.ResponseRecorder) {
	t.Helper()
	ctx, resp := contexttest.MockContext(t, reqPath)
	contexttest.LoadUser(t, ctx, 1) // user1 is the fixture site admin
	if form != nil {
		contexttest.MockRequestPostForm(ctx.Req, form)
	}
	return ctx, resp
}

// upstreamForm is a fully populated, valid submission except for the Target_Owner, which each
// test sets (or deliberately omits) itself.
func upstreamForm(name string) url.Values {
	return url.Values{
		"type":          {string(packages_model.TypeContainer)},
		"name":          {name},
		"url":           {"https://registry-1.docker.io"},
		"mode":          {string(packages_model.UpstreamModePullThrough)},
		"auth_type":     {string(packages_model.UpstreamAuthBasic)},
		"auth_username": {"robot"},
		"auth_secret":   {"s3cret"},
		"metadata_ttl":  {"600"},
		"priority":      {"20"},
		"enabled":       {"on"},
		"remote_prefix": {"library"},
		"pinned_tags":   {`[{"image":"mongo","tag":"latest","target":"4.4.29"}]`},
	}
}

func allUpstreams(t *testing.T, ctx *context.Context) []*packages_model.PackageRegistryUpstream {
	t.Helper()
	ups, err := packages_model.GetAllUpstreams(ctx)
	require.NoError(t, err)
	return ups
}

// insertUpstream seeds a row directly through the model, for the list/edit/delete tests.
func insertUpstream(t *testing.T, ctx *context.Context, ownerID int64, name string) *packages_model.PackageRegistryUpstream {
	t.Helper()
	u, err := packages_model.InsertUpstream(ctx, &packages_model.PackageRegistryUpstream{
		TargetOwnerID: ownerID,
		Type:          packages_model.TypeContainer,
		Name:          name,
		URL:           "https://registry-1.docker.io",
		Mode:          packages_model.UpstreamModePullThrough,
		AuthType:      packages_model.UpstreamAuthNone,
		MetadataTTL:   900,
		Priority:      100,
		Enabled:       true,
		PinnedTags:    []*packages_model.UpstreamPin{{Image: "mongo", Tag: "latest", Target: "4.4.29"}},
	})
	require.NoError(t, err)
	return u
}

// --- happy paths (Requirements 1.3 - 1.6) --------------------------------------------------------

// Requirement 1.3: a created upstream persists every configured field, including the Target_Owner.
func TestPackagesUpstreamsPost_CreatesUpstreamWithAllFields(t *testing.T) {
	unittest.PrepareTestEnv(t)
	enableUpstreamProxy(t)

	form := upstreamForm("dockerhub")
	form.Set("target_owner_id", strconv.FormatInt(fixtureOwnerOrg3, 10))
	ctx, _ := mockAdminContext(t, "POST /-/admin/packages/upstreams", form)

	PackagesUpstreamsPost(ctx)

	assert.Empty(t, string(ctx.Flash.ErrorMsg))
	assert.Contains(t, string(ctx.Flash.SuccessMsg), "admin.packages.upstreams.add_success")

	ups := allUpstreams(t, ctx)
	require.Len(t, ups, 1)
	u := ups[0]

	assert.Equal(t, fixtureOwnerOrg3, u.TargetOwnerID)
	// The model invariant: the target owner is the scoping owner.
	assert.Equal(t, u.TargetOwnerID, u.OwnerID)
	assert.Equal(t, packages_model.TypeContainer, u.Type)
	assert.Equal(t, "dockerhub", u.Name)
	assert.Equal(t, "https://registry-1.docker.io", u.URL)
	assert.Equal(t, packages_model.UpstreamModePullThrough, u.Mode)
	assert.Equal(t, packages_model.UpstreamAuthBasic, u.AuthType)
	assert.Equal(t, "robot", u.AuthUsername)
	assert.Equal(t, "s3cret", u.AuthSecret)
	assert.EqualValues(t, 600, u.MetadataTTL)
	assert.EqualValues(t, 20, u.Priority)
	assert.True(t, u.Enabled)
	assert.Equal(t, "library", u.RemotePrefix)
	// Created through the admin panel, so the provenance flag is set.
	assert.True(t, u.IsAdminManaged)
	require.Len(t, u.PinnedTags, 1)
	assert.Equal(t, packages_model.UpstreamPin{Image: "mongo", Tag: "latest", Target: "4.4.29"}, *u.PinnedTags[0])
}

// Requirement 2.2: the Target_Owner may also be given by owner name instead of the selector id.
func TestPackagesUpstreamsPost_AcceptsTargetOwnerByName(t *testing.T) {
	unittest.PrepareTestEnv(t)
	enableUpstreamProxy(t)

	form := upstreamForm("dockerhub")
	form.Set("target_owner", "org3")
	ctx, _ := mockAdminContext(t, "POST /-/admin/packages/upstreams", form)

	PackagesUpstreamsPost(ctx)

	assert.Empty(t, string(ctx.Flash.ErrorMsg))
	ups := allUpstreams(t, ctx)
	require.Len(t, ups, 1)
	assert.Equal(t, fixtureOwnerOrg3, ups[0].TargetOwnerID)
	assert.Equal(t, fixtureOwnerOrg3, ups[0].OwnerID)
}

// Requirement 1.4: the list shows the upstreams of *all* owners, plus the selector options.
func TestPackagesUpstreams_ListsUpstreamsOfAllOwners(t *testing.T) {
	unittest.PrepareTestEnv(t)
	enableUpstreamProxy(t)

	ctxSeed, _ := mockAdminContext(t, "GET /-/admin/packages/upstreams", nil)
	first := insertUpstream(t, ctxSeed, fixtureOwnerUser2, "hub-user2")
	second := insertUpstream(t, ctxSeed, fixtureOwnerOrg3, "hub-org3")

	ctx, _ := mockAdminContext(t, "GET /-/admin/packages/upstreams", nil)
	PackagesUpstreams(ctx)

	listed, ok := ctx.Data["Upstreams"].([]*packages_model.PackageRegistryUpstream)
	require.True(t, ok, "Upstreams must be in the template data")
	require.Len(t, listed, 2)
	// GetAllUpstreams orders by owner, so user2's row comes before org3's.
	assert.Equal(t, first.ID, listed[0].ID)
	assert.Equal(t, second.ID, listed[1].ID)

	// The Target_Owner selector is populated from existing owners, and every listed row's owner
	// is resolvable for display.
	owners, ok := ctx.Data["AvailableOwners"].([]*user_model.User)
	require.True(t, ok, "AvailableOwners must be in the template data")
	assert.NotEmpty(t, owners)

	byID, ok := ctx.Data["UpstreamOwners"].(map[int64]*user_model.User)
	require.True(t, ok, "UpstreamOwners must be in the template data")
	require.Contains(t, byID, fixtureOwnerUser2)
	require.Contains(t, byID, fixtureOwnerOrg3)
	assert.Equal(t, "user2", byID[fixtureOwnerUser2].Name)
	assert.Equal(t, "org3", byID[fixtureOwnerOrg3].Name)

	// No row was addressed, so the form renders empty.
	assert.Nil(t, ctx.Data["EditUpstream"])
}

// Requirement 1.5 (form population half): ?id=<id> preloads the addressed row for editing.
func TestPackagesUpstreams_PreloadsAddressedUpstreamForEditing(t *testing.T) {
	unittest.PrepareTestEnv(t)
	enableUpstreamProxy(t)

	ctxSeed, _ := mockAdminContext(t, "GET /-/admin/packages/upstreams", nil)
	seeded := insertUpstream(t, ctxSeed, fixtureOwnerOrg3, "hub-org3")

	ctx, _ := mockAdminContext(t, "GET /-/admin/packages/upstreams?id="+strconv.FormatInt(seeded.ID, 10), nil)
	PackagesUpstreams(ctx)

	edit, ok := ctx.Data["EditUpstream"].(*packages_model.PackageRegistryUpstream)
	require.True(t, ok, "EditUpstream must be in the template data")
	assert.Equal(t, seeded.ID, edit.ID)
	pinnedJSON, ok := ctx.Data["EditPinnedTagsJSON"].(string)
	require.True(t, ok, "EditPinnedTagsJSON must be in the template data")
	assert.Contains(t, pinnedJSON, `"image": "mongo"`)
}

// Requirement 1.5: an edit persists the updated values, keeping the target-owner invariant.
func TestPackagesUpstreamsEditPost_PersistsUpdatedValues(t *testing.T) {
	unittest.PrepareTestEnv(t)
	enableUpstreamProxy(t)

	ctxSeed, _ := mockAdminContext(t, "GET /-/admin/packages/upstreams", nil)
	seeded := insertUpstream(t, ctxSeed, fixtureOwnerUser2, "hub-user2")

	form := url.Values{
		"type":          {string(packages_model.TypeContainer)},
		"name":          {"hub-renamed"},
		"url":           {"https://mirror.example.test"},
		"mode":          {string(packages_model.UpstreamModeFrozen)},
		"auth_type":     {string(packages_model.UpstreamAuthToken)},
		"auth_username": {"ci"},
		"auth_secret":   {"rotated"},
		"metadata_ttl":  {"120"},
		"priority":      {"5"},
		"remote_prefix": {"/noenv/"}, // surrounding slashes are trimmed by the handler
		"pinned_tags":   {`[{"image":"nats","tag":"stable","target":"2.10.9"}]`},
		// "enabled" omitted: an unchecked checkbox disables the upstream.
		"target_owner_id": {strconv.FormatInt(fixtureOwnerOrg3, 10)},
	}
	ctx, _ := mockAdminContext(t, "POST /-/admin/packages/upstreams/"+strconv.FormatInt(seeded.ID, 10), form)
	ctx.SetPathParam("id", strconv.FormatInt(seeded.ID, 10))

	PackagesUpstreamsEditPost(ctx)

	assert.Empty(t, string(ctx.Flash.ErrorMsg))
	assert.Contains(t, string(ctx.Flash.SuccessMsg), "admin.packages.upstreams.update_success")

	got, err := packages_model.GetUpstreamByID(ctx, seeded.ID)
	require.NoError(t, err)
	assert.Equal(t, "hub-renamed", got.Name)
	assert.Equal(t, "https://mirror.example.test", got.URL)
	assert.Equal(t, packages_model.UpstreamModeFrozen, got.Mode)
	assert.Equal(t, packages_model.UpstreamAuthToken, got.AuthType)
	assert.Equal(t, "ci", got.AuthUsername)
	assert.Equal(t, "rotated", got.AuthSecret)
	assert.EqualValues(t, 120, got.MetadataTTL)
	assert.EqualValues(t, 5, got.Priority)
	assert.False(t, got.Enabled)
	assert.Equal(t, "noenv", got.RemotePrefix)
	assert.Equal(t, fixtureOwnerOrg3, got.TargetOwnerID)
	assert.Equal(t, got.TargetOwnerID, got.OwnerID)
	assert.True(t, got.IsAdminManaged)
	require.Len(t, got.PinnedTags, 1)
	assert.Equal(t, packages_model.UpstreamPin{Image: "nats", Tag: "stable", Target: "2.10.9"}, *got.PinnedTags[0])
}

// Requirement 1.6: a delete removes the addressed upstream and leaves the others alone.
func TestPackagesUpstreamsDelete_RemovesOnlyAddressedUpstream(t *testing.T) {
	unittest.PrepareTestEnv(t)
	enableUpstreamProxy(t)

	ctxSeed, _ := mockAdminContext(t, "GET /-/admin/packages/upstreams", nil)
	doomed := insertUpstream(t, ctxSeed, fixtureOwnerUser2, "hub-user2")
	kept := insertUpstream(t, ctxSeed, fixtureOwnerOrg3, "hub-org3")

	ctx, _ := mockAdminContext(t, "POST /-/admin/packages/upstreams/"+strconv.FormatInt(doomed.ID, 10)+"/delete", nil)
	ctx.SetPathParam("id", strconv.FormatInt(doomed.ID, 10))

	PackagesUpstreamsDelete(ctx)

	assert.Empty(t, string(ctx.Flash.ErrorMsg))
	assert.Contains(t, string(ctx.Flash.SuccessMsg), "admin.packages.upstreams.delete_success")

	_, err := packages_model.GetUpstreamByID(ctx, doomed.ID)
	assert.ErrorIs(t, err, packages_model.ErrPackageRegistryUpstreamNotExist)

	ups := allUpstreams(t, ctx)
	require.Len(t, ups, 1)
	assert.Equal(t, kept.ID, ups[0].ID)
}

// --- validation: nothing is persisted on a rejected submission (Requirements 2.1, 2.2, 2.4) -----

// Requirement 2.4: a create without a Target_Owner is rejected and persists nothing.
func TestPackagesUpstreamsPost_RejectsMissingTargetOwner(t *testing.T) {
	unittest.PrepareTestEnv(t)
	enableUpstreamProxy(t)

	form := upstreamForm("dockerhub") // no target_owner_id / target_owner at all
	ctx, _ := mockAdminContext(t, "POST /-/admin/packages/upstreams", form)

	PackagesUpstreamsPost(ctx)

	assert.Contains(t, string(ctx.Flash.ErrorMsg), "admin.packages.upstreams.target_owner_required")
	assert.Empty(t, string(ctx.Flash.SuccessMsg))
	assert.Empty(t, allUpstreams(t, ctx))
}

// Requirements 2.2 + 2.4: a create naming an owner that does not exist is rejected, whether the
// owner is given as an id or as a name, and persists nothing.
func TestPackagesUpstreamsPost_RejectsNonExistentTargetOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value string
	}{
		{"by id", "target_owner_id", strconv.FormatInt(missingOwnerID, 10)},
		{"by name", "target_owner", "no-such-owner"},
		{"non-numeric id", "target_owner_id", "not-an-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unittest.PrepareTestEnv(t)
			enableUpstreamProxy(t)

			form := upstreamForm("dockerhub")
			form.Set(tc.field, tc.value)
			ctx, _ := mockAdminContext(t, "POST /-/admin/packages/upstreams", form)

			PackagesUpstreamsPost(ctx)

			assert.Contains(t, string(ctx.Flash.ErrorMsg), "admin.packages.upstreams.target_owner_not_found")
			// The rejected value is named back to the admin.
			assert.Contains(t, string(ctx.Flash.ErrorMsg), tc.value)
			assert.Empty(t, string(ctx.Flash.SuccessMsg))
			assert.Empty(t, allUpstreams(t, ctx))
		})
	}
}

// Invalid pinned-tags JSON is rejected on create with nothing persisted (design: "Invalid
// pinned-tags JSON rejection").
func TestPackagesUpstreamsPost_RejectsInvalidPinnedTagsJSON(t *testing.T) {
	unittest.PrepareTestEnv(t)
	enableUpstreamProxy(t)

	form := upstreamForm("dockerhub")
	form.Set("target_owner_id", strconv.FormatInt(fixtureOwnerOrg3, 10))
	form.Set("pinned_tags", `{"image": "mongo"`) // truncated object, not a JSON array
	ctx, _ := mockAdminContext(t, "POST /-/admin/packages/upstreams", form)

	PackagesUpstreamsPost(ctx)

	assert.Contains(t, string(ctx.Flash.ErrorMsg), "admin.packages.upstreams.pinned_tags_invalid")
	assert.Empty(t, string(ctx.Flash.SuccessMsg))
	assert.Empty(t, allUpstreams(t, ctx))
}

// Requirements 2.2, 2.4 on the edit path: a rejected edit leaves the stored row untouched, so a
// bad submission cannot half-apply.
func TestPackagesUpstreamsEditPost_RejectionLeavesRowUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(url.Values)
		wantErr string
	}{
		{
			name:    "non-existent target owner",
			mutate:  func(f url.Values) { f.Set("target_owner_id", strconv.FormatInt(missingOwnerID, 10)) },
			wantErr: "admin.packages.upstreams.target_owner_not_found",
		},
		{
			name:    "invalid pinned tags",
			mutate:  func(f url.Values) { f.Set("pinned_tags", "definitely not json") },
			wantErr: "admin.packages.upstreams.pinned_tags_invalid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unittest.PrepareTestEnv(t)
			enableUpstreamProxy(t)

			ctxSeed, _ := mockAdminContext(t, "GET /-/admin/packages/upstreams", nil)
			seeded := insertUpstream(t, ctxSeed, fixtureOwnerUser2, "hub-user2")

			form := upstreamForm("hub-renamed")
			form.Set("target_owner_id", strconv.FormatInt(fixtureOwnerOrg3, 10))
			tc.mutate(form)

			ctx, _ := mockAdminContext(t, "POST /-/admin/packages/upstreams/"+strconv.FormatInt(seeded.ID, 10), form)
			ctx.SetPathParam("id", strconv.FormatInt(seeded.ID, 10))

			PackagesUpstreamsEditPost(ctx)

			assert.Contains(t, string(ctx.Flash.ErrorMsg), tc.wantErr)
			assert.Empty(t, string(ctx.Flash.SuccessMsg))

			got, err := packages_model.GetUpstreamByID(ctx, seeded.ID)
			require.NoError(t, err)
			assert.Equal(t, seeded.Name, got.Name)
			assert.Equal(t, seeded.URL, got.URL)
			assert.Equal(t, seeded.Mode, got.Mode)
			assert.Equal(t, seeded.AuthType, got.AuthType)
			assert.Equal(t, seeded.MetadataTTL, got.MetadataTTL)
			assert.Equal(t, seeded.Priority, got.Priority)
			assert.Equal(t, seeded.RemotePrefix, got.RemotePrefix)
			assert.Equal(t, fixtureOwnerUser2, got.OwnerID)
			assert.Equal(t, fixtureOwnerUser2, got.TargetOwnerID)
			require.Len(t, got.PinnedTags, 1)
			assert.Equal(t, "mongo", got.PinnedTags[0].Image)
		})
	}
}

// --- helpers ------------------------------------------------------------------------------------

// parsePinnedTags is the shared textarea parser: blank clears the pins, valid JSON round-trips,
// invalid JSON returns a display error and no pins (so the caller persists nothing).
func TestParsePinnedTags(t *testing.T) {
	t.Run("blank input clears the pins", func(t *testing.T) {
		for _, raw := range []string{"", "   ", "\n\t "} {
			pins, errMsg := parsePinnedTags(raw)
			assert.Empty(t, errMsg)
			assert.Nil(t, pins)
		}
	})

	t.Run("valid JSON array parses", func(t *testing.T) {
		pins, errMsg := parsePinnedTags(`
			[
			  {"image":"library/mongo","tag":"latest","target":"4.4.29"},
			  {"image":"noenv/nats","tag":"stable","target":"2.10.9"}
			]`)
		require.Empty(t, errMsg)
		require.Len(t, pins, 2)
		assert.Equal(t, packages_model.UpstreamPin{Image: "library/mongo", Tag: "latest", Target: "4.4.29"}, *pins[0])
		assert.Equal(t, packages_model.UpstreamPin{Image: "noenv/nats", Tag: "stable", Target: "2.10.9"}, *pins[1])
	})

	t.Run("invalid JSON is rejected with no pins", func(t *testing.T) {
		for _, raw := range []string{
			`{"image":"mongo","tag":"latest","target":"4.4.29"}`, // object, not an array
			`[{"image":"mongo"`, // truncated
			`not json at all`,
		} {
			pins, errMsg := parsePinnedTags(raw)
			assert.NotEmpty(t, errMsg, "input %q must be rejected", raw)
			assert.Nil(t, pins)
		}
	})
}

// Requirement 1.7 / 9.2 (defensive half): with the feature flag off the handlers are inert. The
// routes are not registered at all in that case - route absence is covered by the integration
// tests - but the in-handler guard must still refuse to act.
func TestPackagesUpstreams_FlagOffHandlersAreInert(t *testing.T) {
	unittest.PrepareTestEnv(t)
	setting.Packages.EnableUpstreamProxy = false

	form := upstreamForm("dockerhub")
	form.Set("target_owner_id", strconv.FormatInt(fixtureOwnerOrg3, 10))
	ctx, resp := mockAdminContext(t, "POST /-/admin/packages/upstreams", form)

	PackagesUpstreamsPost(ctx)

	assert.Equal(t, 404, resp.Code)
	assert.Empty(t, allUpstreams(t, ctx))
}
