// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package upstream provides an owner-scoped management API for package registry upstreams
// (the pull-through / freeze proxy configuration). Mounted at /api/packages/{owner}/-/upstreams.
package upstream

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/services/context"
)

// supportedTypes limits the PoC to the formats we implement a proxy hook for.
var supportedTypes = map[packages_model.Type]bool{
	packages_model.TypeMaven:     true,
	packages_model.TypeContainer: true,
}

type upstreamResponse struct {
	ID           int64                         `json:"id"`
	OwnerID      int64                         `json:"owner_id"`
	Type         string                        `json:"type"`
	Name         string                        `json:"name"`
	URL          string                        `json:"url"`
	Mode         string                        `json:"mode"`
	AuthType     string                        `json:"auth_type"`
	AuthUsername string                        `json:"auth_username"`
	MetadataTTL  int64                         `json:"metadata_ttl"`
	Priority     int64                         `json:"priority"`
	Enabled      bool                          `json:"enabled"`
	FetchCount   int64                         `json:"fetch_count"`
	HitCount     int64                         `json:"hit_count"`
	PinnedTags   []*packages_model.UpstreamPin `json:"pinned_tags"`
	Created      int64                         `json:"created"`
	Updated      int64                         `json:"updated"`
}

func toResponse(u *packages_model.PackageRegistryUpstream) *upstreamResponse {
	return &upstreamResponse{
		ID:           u.ID,
		OwnerID:      u.OwnerID,
		Type:         string(u.Type),
		Name:         u.Name,
		URL:          u.URL,
		Mode:         string(u.Mode),
		AuthType:     string(u.AuthType),
		AuthUsername: u.AuthUsername,
		MetadataTTL:  u.MetadataTTL,
		Priority:     u.Priority,
		Enabled:      u.Enabled,
		FetchCount:   u.FetchCount,
		HitCount:     u.HitCount,
		PinnedTags:   u.PinnedTags,
		Created:      int64(u.CreatedUnix),
		Updated:      int64(u.UpdatedUnix),
	}
}

type upstreamRequest struct {
	Type         string                        `json:"type"`
	Name         string                        `json:"name"`
	URL          string                        `json:"url"`
	Mode         string                        `json:"mode"`
	AuthType     string                        `json:"auth_type"`
	AuthUsername string                        `json:"auth_username"`
	AuthSecret   string                        `json:"auth_secret"`
	MetadataTTL  int64                         `json:"metadata_ttl"`
	Priority     *int64                        `json:"priority"`
	Enabled      *bool                         `json:"enabled"`
	PinnedTags   []*packages_model.UpstreamPin `json:"pinned_tags"`
}

func apiError(ctx *context.Context, status int, msg string) {
	ctx.JSON(status, map[string]string{"message": msg})
}

// List returns all upstreams configured for the owner.
func List(ctx *context.Context) {
	ups, err := packages_model.GetUpstreamsByOwner(ctx, ctx.Package.Owner.ID)
	if err != nil {
		apiError(ctx, http.StatusInternalServerError, err.Error())
		return
	}
	res := make([]*upstreamResponse, 0, len(ups))
	for _, u := range ups {
		res = append(res, toResponse(u))
	}
	ctx.JSON(http.StatusOK, res)
}

// Create adds a new upstream for the owner.
func Create(ctx *context.Context) {
	var req upstreamRequest
	if err := json.NewDecoder(ctx.Req.Body).Decode(&req); err != nil {
		apiError(ctx, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	pkgType := packages_model.Type(strings.ToLower(strings.TrimSpace(req.Type)))
	if !supportedTypes[pkgType] {
		apiError(ctx, http.StatusBadRequest, "unsupported or missing type (PoC supports: maven, container)")
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.URL) == "" {
		apiError(ctx, http.StatusBadRequest, "name and url are required")
		return
	}

	mode := packages_model.UpstreamMode(req.Mode)
	if mode == "" {
		mode = packages_model.UpstreamModePullThrough
	}
	if !mode.IsValid() {
		apiError(ctx, http.StatusBadRequest, "mode must be 'pull_through' or 'frozen'")
		return
	}

	authType := packages_model.UpstreamAuthType(req.AuthType)
	if authType == "" {
		authType = packages_model.UpstreamAuthNone
	}
	if !authType.IsValid() {
		apiError(ctx, http.StatusBadRequest, "auth_type must be 'none', 'basic' or 'token'")
		return
	}

	ttl := req.MetadataTTL
	if ttl <= 0 {
		ttl = 900
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	priority := int64(100)
	if req.Priority != nil {
		priority = *req.Priority
	}

	u := &packages_model.PackageRegistryUpstream{
		OwnerID:      ctx.Package.Owner.ID,
		Type:         pkgType,
		Name:         strings.TrimSpace(req.Name),
		URL:          strings.TrimSpace(req.URL),
		Mode:         mode,
		AuthType:     authType,
		AuthUsername: req.AuthUsername,
		AuthSecret:   req.AuthSecret,
		MetadataTTL:  ttl,
		Priority:     priority,
		Enabled:      enabled,
		PinnedTags:   req.PinnedTags,
	}
	if _, err := packages_model.InsertUpstream(ctx, u); err != nil {
		apiError(ctx, http.StatusInternalServerError, err.Error())
		return
	}
	ctx.JSON(http.StatusCreated, toResponse(u))
}

// getOwned loads an upstream by path id and verifies it belongs to the owner in context.
func getOwned(ctx *context.Context) *packages_model.PackageRegistryUpstream {
	id, err := strconv.ParseInt(ctx.PathParam("id"), 10, 64)
	if err != nil {
		apiError(ctx, http.StatusBadRequest, "invalid id")
		return nil
	}
	u, err := packages_model.GetUpstreamByID(ctx, id)
	if err != nil {
		apiError(ctx, http.StatusNotFound, "upstream not found")
		return nil
	}
	if u.OwnerID != ctx.Package.Owner.ID {
		apiError(ctx, http.StatusNotFound, "upstream not found")
		return nil
	}
	return u
}

// Get returns a single upstream.
func Get(ctx *context.Context) {
	u := getOwned(ctx)
	if u == nil {
		return
	}
	ctx.JSON(http.StatusOK, toResponse(u))
}

// Update patches mutable fields of an upstream.
func Update(ctx *context.Context) {
	u := getOwned(ctx)
	if u == nil {
		return
	}
	var req upstreamRequest
	if err := json.NewDecoder(ctx.Req.Body).Decode(&req); err != nil {
		apiError(ctx, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	if req.URL != "" {
		u.URL = strings.TrimSpace(req.URL)
	}
	if req.Name != "" {
		u.Name = strings.TrimSpace(req.Name)
	}
	if req.Mode != "" {
		mode := packages_model.UpstreamMode(req.Mode)
		if !mode.IsValid() {
			apiError(ctx, http.StatusBadRequest, "mode must be 'pull_through' or 'frozen'")
			return
		}
		u.Mode = mode
	}
	if req.AuthType != "" {
		at := packages_model.UpstreamAuthType(req.AuthType)
		if !at.IsValid() {
			apiError(ctx, http.StatusBadRequest, "auth_type must be 'none', 'basic' or 'token'")
			return
		}
		u.AuthType = at
	}
	if req.AuthUsername != "" {
		u.AuthUsername = req.AuthUsername
	}
	if req.AuthSecret != "" {
		u.AuthSecret = req.AuthSecret
	}
	if req.MetadataTTL > 0 {
		u.MetadataTTL = req.MetadataTTL
	}
	if req.Enabled != nil {
		u.Enabled = *req.Enabled
	}
	if req.Priority != nil {
		u.Priority = *req.Priority
	}
	if req.PinnedTags != nil {
		u.PinnedTags = req.PinnedTags
	}

	if err := packages_model.UpdateUpstream(ctx, u); err != nil {
		apiError(ctx, http.StatusInternalServerError, err.Error())
		return
	}
	ctx.JSON(http.StatusOK, toResponse(u))
}

// Delete removes an upstream.
func Delete(ctx *context.Context) {
	u := getOwned(ctx)
	if u == nil {
		return
	}
	if err := packages_model.DeleteUpstreamByID(ctx, u.ID); err != nil {
		apiError(ctx, http.StatusInternalServerError, err.Error())
		return
	}
	ctx.Status(http.StatusNoContent)
}
