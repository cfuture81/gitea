// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package admin

import (
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"gitea.dev/models/db"
	packages_model "gitea.dev/models/packages"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/structs"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

// The site-admin registry upstream page. This is the single configuration surface for the
// pull-through / freeze proxy: it edits package_registry_upstream rows across *all* owners, with
// the target owner (the owner/org that receives the cached packages) chosen explicitly per row.
//
// Template data contract for templates/admin/packages/upstreams.tmpl:
//
//	Title, PageIsAdminPackages, PageIsAdminPackagesUpstreams - admin layout / navbar
//	EnableUpstreamProxy bool                                     - feature flag (always true here)
//	UpstreamsLink       string                                   - form/action base URL
//	Upstreams           []*packages_model.PackageRegistryUpstream - every row, all owners
//	UpstreamOwners      map[int64]*user_model.User               - owner id -> owner, for the list
//	AvailableOwners     []*user_model.User                       - Target_Owner selector options
//	EditUpstream        *packages_model.PackageRegistryUpstream  - set when ?id=<id> is given
//	EditPinnedTagsJSON  string                                   - pretty-printed pins for the textarea
//
// The form posts the Target_Owner as "target_owner_id" (the selector's owner id); a "target_owner"
// owner name is accepted as an alternative.
const tplPackagesUpstreams templates.TplName = "admin/packages/upstreams"

// upstreamProxyTypes are the package types that have a proxy implementation
// (routers/api/packages/*/proxy.go), i.e. the types an upstream can usefully be configured for.
var upstreamProxyTypes = []packages_model.Type{
	packages_model.TypeMaven,
	packages_model.TypeContainer,
	packages_model.TypeNpm,
	packages_model.TypePyPI,
}

func packagesUpstreamsLink() string {
	return setting.AppSubURL + "/-/admin/packages/upstreams"
}

// parsePinnedTags reads the textarea JSON (a list of {image,tag,target}) into model pins.
// Empty/blank input clears the pins. Invalid JSON returns an error string for display, so a bad
// submission is rejected without partial persistence.
func parsePinnedTags(raw string) ([]*packages_model.UpstreamPin, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	var pins []*packages_model.UpstreamPin
	if err := json.Unmarshal([]byte(raw), &pins); err != nil {
		return nil, "pinned tags must be a JSON array of {\"image\",\"tag\",\"target\"}: " + err.Error()
	}
	return pins, ""
}

// requireUpstreamProxy is a defensive feature-flag guard. The routes are only registered when the
// flag is on, so with the flag off the page does not exist at all; this keeps every handler inert
// even if one is reached some other way.
func requireUpstreamProxy(ctx *context.Context) bool {
	if !setting.Packages.EnableUpstreamProxy {
		ctx.NotFound(nil)
		return false
	}
	return true
}

// listUpstreamOwners returns every user and organization, i.e. the owners selectable as an
// upstream's Target_Owner. The doer is a site admin, so all visibilities are included.
func listUpstreamOwners(ctx *context.Context) ([]*user_model.User, error) {
	owners, _, err := user_model.SearchUsers(ctx, user_model.SearchUserOptions{
		Actor:       ctx.Doer,
		Types:       []user_model.UserType{user_model.UserTypeIndividual, user_model.UserTypeOrganization},
		Visible:     []structs.VisibleType{structs.VisibleTypePublic, structs.VisibleTypeLimited, structs.VisibleTypePrivate},
		OrderBy:     db.SearchOrderByAlphabetically,
		ListOptions: db.ListOptionsAll,
	})
	return owners, err
}

// resolveTargetOwner reads the submitted Target_Owner and verifies it references an existing Gitea
// owner. It accepts either the selector's numeric "target_owner_id" or a "target_owner" owner name.
//
// fallback is used only when neither field was submitted: 0 on create (so a missing Target_Owner is
// rejected) and the row's current owner on edit. The resulting id is always looked up, so an owner
// that has been deleted since the upstream was configured is reported too.
//
// The second return value is a display error naming the missing/invalid owner, empty when the
// returned id is a valid owner.
func resolveTargetOwner(ctx *context.Context, fallback int64) (int64, template.HTML) {
	rawID := strings.TrimSpace(ctx.FormString("target_owner_id"))
	rawName := strings.TrimSpace(ctx.FormString("target_owner"))

	var id int64
	switch {
	case rawID != "":
		parsed, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil || parsed <= 0 {
			return 0, ctx.Tr("admin.packages.upstreams.target_owner_not_found", rawID)
		}
		id = parsed
	case rawName != "":
		owner, err := user_model.GetUserByName(ctx, rawName)
		if err != nil {
			if user_model.IsErrUserNotExist(err) {
				return 0, ctx.Tr("admin.packages.upstreams.target_owner_not_found", rawName)
			}
			return 0, template.HTML("Target owner " + strconv.Quote(rawName) + " could not be resolved: " + err.Error())
		}
		id = owner.ID
	default:
		id = fallback
	}

	if id <= 0 {
		return 0, ctx.Tr("admin.packages.upstreams.target_owner_required")
	}

	owner, err := user_model.GetUserByID(ctx, id)
	if err != nil {
		if user_model.IsErrUserNotExist(err) {
			return 0, ctx.Tr("admin.packages.upstreams.target_owner_not_found", strconv.FormatInt(id, 10))
		}
		return 0, template.HTML("Target owner with id " + strconv.FormatInt(id, 10) + " could not be resolved: " + err.Error())
	}
	if !owner.IsIndividual() && !owner.IsOrganization() {
		return 0, ctx.Tr("admin.packages.upstreams.target_owner_not_found", owner.Name)
	}
	return owner.ID, ""
}

// loadUpstream loads the upstream addressed by the {id} path parameter. Unlike the owner-scoped
// page there is no ownership check - a site admin manages every owner's rows.
func loadUpstream(ctx *context.Context) *packages_model.PackageRegistryUpstream {
	u, err := packages_model.GetUpstreamByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		ctx.NotFound(err)
		return nil
	}
	return u
}

// PackagesUpstreams renders the list of all configured upstreams plus the add/edit form.
func PackagesUpstreams(ctx *context.Context) {
	if !requireUpstreamProxy(ctx) {
		return
	}

	ctx.Data["Title"] = ctx.Tr("admin.packages.upstreams")
	ctx.Data["PageIsAdminPackages"] = true
	ctx.Data["PageIsAdminPackagesUpstreams"] = true
	ctx.Data["EnableUpstreamProxy"] = setting.Packages.EnableUpstreamProxy
	ctx.Data["UpstreamsLink"] = packagesUpstreamsLink()

	ups, err := packages_model.GetAllUpstreams(ctx)
	if err != nil {
		ctx.ServerError("GetAllUpstreams", err)
		return
	}
	ctx.Data["Upstreams"] = ups

	owners, err := listUpstreamOwners(ctx)
	if err != nil {
		ctx.ServerError("SearchUsers", err)
		return
	}
	ctx.Data["AvailableOwners"] = owners

	// Target owner of every listed row, for display. An owner deleted since the upstream was
	// configured simply has no entry, so the template can flag the dangling id instead.
	upstreamOwners := make(map[int64]*user_model.User, len(owners))
	for _, o := range owners {
		upstreamOwners[o.ID] = o
	}
	ctx.Data["UpstreamOwners"] = upstreamOwners

	if id := ctx.FormInt64("id"); id > 0 {
		u, err := packages_model.GetUpstreamByID(ctx, id)
		if err == nil {
			ctx.Data["EditUpstream"] = u
			if len(u.PinnedTags) > 0 {
				if b, err := json.MarshalIndent(u.PinnedTags, "", "  "); err == nil {
					ctx.Data["EditPinnedTagsJSON"] = string(b)
				}
			}
		}
	}

	ctx.HTML(http.StatusOK, tplPackagesUpstreams)
}

// PackagesUpstreamsPost creates a new admin-managed upstream. Nothing is persisted unless every
// field validates, so a rejected submission leaves the configuration untouched.
func PackagesUpstreamsPost(ctx *context.Context) {
	if !requireUpstreamProxy(ctx) {
		return
	}

	pkgType := packages_model.Type(strings.ToLower(strings.TrimSpace(ctx.FormString("type"))))
	if !slices.Contains(upstreamProxyTypes, pkgType) {
		ctx.Flash.Error("Unsupported package type (supported: maven, container, npm, pypi)")
		ctx.Redirect(packagesUpstreamsLink())
		return
	}
	name := strings.TrimSpace(ctx.FormString("name"))
	url := strings.TrimSpace(ctx.FormString("url"))
	if name == "" || url == "" {
		ctx.Flash.Error("Name and URL are required")
		ctx.Redirect(packagesUpstreamsLink())
		return
	}

	targetOwnerID, ownerErr := resolveTargetOwner(ctx, 0)
	if ownerErr != "" {
		ctx.Flash.Error(ownerErr)
		ctx.Redirect(packagesUpstreamsLink())
		return
	}

	pins, perr := parsePinnedTags(ctx.FormString("pinned_tags"))
	if perr != "" {
		ctx.Flash.Error(ctx.Tr("admin.packages.upstreams.pinned_tags_invalid", perr))
		ctx.Redirect(packagesUpstreamsLink())
		return
	}

	mode := packages_model.UpstreamMode(ctx.FormString("mode"))
	if !mode.IsValid() {
		mode = packages_model.UpstreamModePullThrough
	}
	authType := packages_model.UpstreamAuthType(ctx.FormString("auth_type"))
	if !authType.IsValid() {
		authType = packages_model.UpstreamAuthNone
	}
	ttl := ctx.FormInt64("metadata_ttl")
	if ttl <= 0 {
		ttl = 900
	}
	priority := ctx.FormInt64("priority")
	if priority <= 0 {
		priority = 100
	}

	u := &packages_model.PackageRegistryUpstream{
		// OwnerID is the scoping column the resolver uses; TargetOwnerID is the admin-facing name
		// for it. Both are set so the TargetOwnerID == OwnerID invariant is explicit here as well as
		// in the model's normalizeTargetOwner.
		OwnerID:        targetOwnerID,
		TargetOwnerID:  targetOwnerID,
		Type:           pkgType,
		Name:           name,
		URL:            url,
		Mode:           mode,
		AuthType:       authType,
		AuthUsername:   strings.TrimSpace(ctx.FormString("auth_username")),
		AuthSecret:     ctx.FormString("auth_secret"),
		MetadataTTL:    ttl,
		Priority:       priority,
		Enabled:        ctx.FormBool("enabled"),
		RemotePrefix:   strings.Trim(strings.TrimSpace(ctx.FormString("remote_prefix")), "/"),
		IsAdminManaged: true,
		PinnedTags:     pins,
	}
	if _, err := packages_model.InsertUpstream(ctx, u); err != nil {
		ctx.Flash.Error("Create failed: " + err.Error())
		ctx.Redirect(packagesUpstreamsLink())
		return
	}
	ctx.Flash.Success(ctx.Tr("admin.packages.upstreams.add_success"))
	ctx.Redirect(packagesUpstreamsLink())
}

// PackagesUpstreamsEditPost updates an existing upstream.
func PackagesUpstreamsEditPost(ctx *context.Context) {
	if !requireUpstreamProxy(ctx) {
		return
	}

	u := loadUpstream(ctx)
	if u == nil {
		return
	}
	editLink := packagesUpstreamsLink() + "?id=" + ctx.PathParam("id")

	// Validate everything that can be rejected before mutating u, so a failed edit persists nothing.
	targetOwnerID, ownerErr := resolveTargetOwner(ctx, u.OwnerID)
	if ownerErr != "" {
		ctx.Flash.Error(ownerErr)
		ctx.Redirect(editLink)
		return
	}
	pins, perr := parsePinnedTags(ctx.FormString("pinned_tags"))
	if perr != "" {
		ctx.Flash.Error(ctx.Tr("admin.packages.upstreams.pinned_tags_invalid", perr))
		ctx.Redirect(editLink)
		return
	}

	// Keep TargetOwnerID == OwnerID (also enforced by the model on update).
	u.OwnerID = targetOwnerID
	u.TargetOwnerID = targetOwnerID

	if v := strings.TrimSpace(ctx.FormString("name")); v != "" {
		u.Name = v
	}
	if v := strings.TrimSpace(ctx.FormString("url")); v != "" {
		u.URL = v
	}
	if mode := packages_model.UpstreamMode(ctx.FormString("mode")); mode.IsValid() {
		u.Mode = mode
	}
	if at := packages_model.UpstreamAuthType(ctx.FormString("auth_type")); at.IsValid() {
		u.AuthType = at
	}
	u.AuthUsername = strings.TrimSpace(ctx.FormString("auth_username"))
	if v := ctx.FormString("auth_secret"); v != "" {
		u.AuthSecret = v
	}
	if ttl := ctx.FormInt64("metadata_ttl"); ttl > 0 {
		u.MetadataTTL = ttl
	}
	if prio := ctx.FormInt64("priority"); prio > 0 {
		u.Priority = prio
	}
	u.Enabled = ctx.FormBool("enabled")
	u.RemotePrefix = strings.Trim(strings.TrimSpace(ctx.FormString("remote_prefix")), "/")
	u.PinnedTags = pins
	u.IsAdminManaged = true

	if err := packages_model.UpdateUpstream(ctx, u); err != nil {
		ctx.Flash.Error("Update failed: " + err.Error())
		ctx.Redirect(editLink)
		return
	}
	ctx.Flash.Success(ctx.Tr("admin.packages.upstreams.update_success"))
	ctx.Redirect(packagesUpstreamsLink())
}

// PackagesUpstreamsDelete removes an upstream from the configuration.
func PackagesUpstreamsDelete(ctx *context.Context) {
	if !requireUpstreamProxy(ctx) {
		return
	}

	u := loadUpstream(ctx)
	if u == nil {
		return
	}
	if err := packages_model.DeleteUpstreamByID(ctx, u.ID); err != nil {
		ctx.Flash.Error("Delete failed: " + err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("admin.packages.upstreams.delete_success"))
	}
	ctx.Redirect(packagesUpstreamsLink())
}
