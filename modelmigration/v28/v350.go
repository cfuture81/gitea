// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
)

// AddPinnedTagsToPackageRegistryUpstream adds the pinned_tags JSON column used to alias mutable
// tags (e.g. "latest") to a fixed upstream reference (version freeze).
func AddPinnedTagsToPackageRegistryUpstream(_ context.Context, x base.EngineMigration) error {
	type PackageRegistryUpstream struct {
		ID         int64  `xorm:"pk autoincr"`
		PinnedTags string `xorm:"JSON TEXT"`
	}
	return x.Sync(new(PackageRegistryUpstream))
}
