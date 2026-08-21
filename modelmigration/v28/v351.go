// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
)

// AddPriorityToPackageRegistryUpstream adds the priority column used to order multiple upstreams
// within an (owner, type) group (lower = tried first on a local miss).
func AddPriorityToPackageRegistryUpstream(_ context.Context, x base.EngineMigration) error {
	type PackageRegistryUpstream struct {
		ID       int64 `xorm:"pk autoincr"`
		Priority int64 `xorm:"NOT NULL DEFAULT 100"`
	}
	return x.Sync(new(PackageRegistryUpstream))
}
