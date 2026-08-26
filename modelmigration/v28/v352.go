// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
)

// AddTargetOwnerAndRemotePrefixToPackageRegistryUpstream adds the columns backing admin-managed
// upstream configuration: target_owner_id (the owner/org that receives packages cached through the
// upstream), remote_prefix (the namespace prepended to the image when addressing the upstream) and
// is_admin_managed (provenance of the row, display only).
//
// The migration is strictly additive: it adds columns and never drops or rewrites an existing one.
// Rows written before this migration were configured owner-scoped, where the owner that owns the
// pull path is also the owner receiving the cached packages, so they are backfilled with
// target_owner_id = owner_id and keep resolving exactly as before.
//
// REBASE-COLLISION HOTSPOT: the migration ID 352 continues the fork-local sequence v349-v351 and
// will collide with upstream go-gitea/gitea as soon as upstream claims the same number. On every
// rebase, renumber this migration (file name, doc comment and the newMigration entry in
// modelmigration/migrations.go) to the next free ID after the upstream tail.
func AddTargetOwnerAndRemotePrefixToPackageRegistryUpstream(_ context.Context, x base.EngineMigration) error {
	type PackageRegistryUpstream struct {
		ID             int64  `xorm:"pk autoincr"`
		TargetOwnerID  int64  `xorm:"NOT NULL DEFAULT 0"`
		RemotePrefix   string `xorm:"NOT NULL DEFAULT ''"`
		IsAdminManaged bool   `xorm:"NOT NULL DEFAULT false"`
	}
	if err := x.Sync(new(PackageRegistryUpstream)); err != nil {
		return err
	}

	// Backfill the new target owner from the existing scoping column. Restricted to rows still at
	// the column default so the statement stays idempotent and never rewrites a target owner that
	// was set explicitly.
	_, err := x.Exec("UPDATE package_registry_upstream SET target_owner_id = owner_id WHERE target_owner_id = 0")
	return err
}
