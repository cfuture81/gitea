// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"net/http"
	"strings"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplSettingsPackagesUpstreams templates.TplName = "user/settings/packages_upstreams"

func upstreamsLink() string {
	return setting.AppSubURL + "/user/settings/packages/upstreams"
}

// parsePinnedTags reads the textarea JSON (a list of {image,tag,target}) into model pins.
// Empty/blank input clears the pins. Invalid JSON returns an error string for display.
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

// UpstreamProxies renders the list + add/edit form for the owner's registry upstreams.
func UpstreamProxies(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("packages.title")
	ctx.Data["PageIsSettingsPackages"] = true
	ctx.Data["EnableUpstreamProxy"] = setting.Packages.EnableUpstreamProxy
	ctx.Data["UpstreamsLink"] = upstreamsLink()

	ups, err := packages_model.GetUpstreamsByOwner(ctx, ctx.Doer.ID)
	if err != nil {
		ctx.ServerError("GetUpstreamsByOwner", err)
		return
	}
	ctx.Data["Upstreams"] = ups

	if id := ctx.FormInt64("id"); id > 0 {
		u, err := packages_model.GetUpstreamByID(ctx, id)
		if err == nil && u.OwnerID == ctx.Doer.ID {
			ctx.Data["EditUpstream"] = u
			if len(u.PinnedTags) > 0 {
				if b, err := json.MarshalIndent(u.PinnedTags, "", "  "); err == nil {
					ctx.Data["EditPinnedTagsJSON"] = string(b)
				}
			}
		}
	}

	ctx.HTML(http.StatusOK, tplSettingsPackagesUpstreams)
}

// UpstreamProxiesPost creates a new upstream from the add form.
func UpstreamProxiesPost(ctx *context.Context) {
	pkgType := packages_model.Type(strings.ToLower(strings.TrimSpace(ctx.FormString("type"))))
	if pkgType != packages_model.TypeMaven && pkgType != packages_model.TypeContainer {
		ctx.Flash.Error("Unsupported type (PoC supports: maven, container)")
		ctx.Redirect(upstreamsLink())
		return
	}
	name := strings.TrimSpace(ctx.FormString("name"))
	url := strings.TrimSpace(ctx.FormString("url"))
	if name == "" || url == "" {
		ctx.Flash.Error("Name and URL are required")
		ctx.Redirect(upstreamsLink())
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
	pins, perr := parsePinnedTags(ctx.FormString("pinned_tags"))
	if perr != "" {
		ctx.Flash.Error(perr)
		ctx.Redirect(upstreamsLink())
		return
	}

	u := &packages_model.PackageRegistryUpstream{
		OwnerID:      ctx.Doer.ID,
		Type:         pkgType,
		Name:         name,
		URL:          url,
		Mode:         mode,
		AuthType:     authType,
		AuthUsername: strings.TrimSpace(ctx.FormString("auth_username")),
		AuthSecret:   ctx.FormString("auth_secret"),
		MetadataTTL:  ttl,
		Priority:     priority,
		Enabled:      ctx.FormBool("enabled"),
		PinnedTags:   pins,
	}
	if _, err := packages_model.InsertUpstream(ctx, u); err != nil {
		ctx.Flash.Error("Create failed: " + err.Error())
		ctx.Redirect(upstreamsLink())
		return
	}
	ctx.Flash.Success("Upstream created")
	ctx.Redirect(upstreamsLink())
}

func loadOwnedUpstream(ctx *context.Context) *packages_model.PackageRegistryUpstream {
	u, err := packages_model.GetUpstreamByID(ctx, ctx.PathParamInt64("id"))
	if err != nil || u.OwnerID != ctx.Doer.ID {
		ctx.NotFound(nil)
		return nil
	}
	return u
}

// UpstreamProxiesEditPost updates an existing upstream.
func UpstreamProxiesEditPost(ctx *context.Context) {
	u := loadOwnedUpstream(ctx)
	if u == nil {
		return
	}

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

	pins, perr := parsePinnedTags(ctx.FormString("pinned_tags"))
	if perr != "" {
		ctx.Flash.Error(perr)
		ctx.Redirect(upstreamsLink() + "?id=" + ctx.PathParam("id"))
		return
	}
	u.PinnedTags = pins

	if err := packages_model.UpdateUpstream(ctx, u); err != nil {
		ctx.Flash.Error("Update failed: " + err.Error())
		ctx.Redirect(upstreamsLink() + "?id=" + ctx.PathParam("id"))
		return
	}
	ctx.Flash.Success("Upstream updated")
	ctx.Redirect(upstreamsLink())
}

// UpstreamProxiesDelete removes an upstream.
func UpstreamProxiesDelete(ctx *context.Context) {
	u := loadOwnedUpstream(ctx)
	if u == nil {
		return
	}
	if err := packages_model.DeleteUpstreamByID(ctx, u.ID); err != nil {
		ctx.Flash.Error("Delete failed: " + err.Error())
	} else {
		ctx.Flash.Success("Upstream deleted")
	}
	ctx.Redirect(upstreamsLink())
}
