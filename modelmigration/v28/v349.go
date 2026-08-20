// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"

	"xorm.io/xorm/schemas"
)

// PackageRegistryUpstream is a migration-local copy of the model (migrations must not depend on
// the live model definition).
type PackageRegistryUpstream struct {
	ID           int64  `xorm:"pk autoincr"`
	OwnerID      int64  `xorm:"INDEX NOT NULL"`
	Type         string `xorm:"INDEX NOT NULL"`
	Name         string `xorm:"NOT NULL"`
	URL          string `xorm:"NOT NULL"`
	Mode         string `xorm:"NOT NULL DEFAULT 'pull_through'"`
	AuthType     string `xorm:"NOT NULL DEFAULT 'none'"`
	AuthUsername string `xorm:"NOT NULL DEFAULT ''"`
	AuthSecret   string `xorm:"NOT NULL DEFAULT ''"`
	MetadataTTL  int64  `xorm:"NOT NULL DEFAULT 900"`
	Enabled      bool   `xorm:"INDEX NOT NULL DEFAULT true"`
	FetchCount   int64  `xorm:"NOT NULL DEFAULT 0"`
	HitCount     int64  `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix  int64  `xorm:"created NOT NULL DEFAULT 0"`
	UpdatedUnix  int64  `xorm:"updated NOT NULL DEFAULT 0"`
}

// TableIndices implements xorm's TableIndices interface: a composite unique index over
// (owner_id, type, name).
func (u *PackageRegistryUpstream) TableIndices() []*schemas.Index {
	unique := schemas.NewIndex("unique_owner_type_name", schemas.UniqueType)
	unique.AddColumn("owner_id", "type", "name")
	return []*schemas.Index{unique}
}

// AddPackageRegistryUpstream creates the package_registry_upstream table.
func AddPackageRegistryUpstream(_ context.Context, x base.EngineMigration) error {
	return x.Sync(new(PackageRegistryUpstream))
}
