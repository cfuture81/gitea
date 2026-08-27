// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package packages

import (
	"net/http"

	"gitea.dev/services/context"
)

// reqSiteAdmin restricts a route to site administrators.
//
// It exists for the registry-upstream management API. That API is proxy *configuration*, not
// package content, and configuration is owned by the site admin panel
// (`/-/admin/packages/upstreams`), so the API must be gated to the same audience as the panel
// (Requirement 1.2) — package write access is not enough, because any org member on a team with
// Packages:write would satisfy it.
//
// This is deliberately kept next to the feature rather than folded into api.go: api.go is an
// upstream-owned file and the fork's hygiene rule is to touch it with as few lines as possible.
//
// Ordering note: on the upstream routes this runs *after* reqPackageAccess, which already answers
// 401 (with a WWW-Authenticate challenge) for an anonymous or insufficiently-scoped caller. Every
// request that reaches here is therefore authenticated, and a plain 403 is the correct answer.
func reqSiteAdmin(ctx *context.Context) {
	if !ctx.IsUserSiteAdmin() {
		ctx.HTTPError(http.StatusForbidden, "reqSiteAdmin", "user must be a site administrator")
		return
	}
}
