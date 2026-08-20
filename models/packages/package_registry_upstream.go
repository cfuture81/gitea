// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package packages

import (
	"context"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
)

var ErrPackageRegistryUpstreamNotExist = util.NewNotExistErrorf("package registry upstream does not exist")

func init() {
	db.RegisterModel(new(PackageRegistryUpstream))
}

// UpstreamMode controls how a configured upstream is served.
type UpstreamMode string

const (
	// UpstreamModePullThrough fetches on a miss and revalidates mutable content per MetadataTTL.
	UpstreamModePullThrough UpstreamMode = "pull_through"
	// UpstreamModeFrozen serves exactly what is cached and never revalidates/refetches mutable
	// content (the version-control / freeze behaviour).
	UpstreamModeFrozen UpstreamMode = "frozen"
)

func (m UpstreamMode) IsValid() bool {
	return m == UpstreamModePullThrough || m == UpstreamModeFrozen
}

// UpstreamAuthType is the auth scheme used against the upstream registry.
type UpstreamAuthType string

const (
	UpstreamAuthNone  UpstreamAuthType = "none"
	UpstreamAuthBasic UpstreamAuthType = "basic"
	UpstreamAuthToken UpstreamAuthType = "token"
)

func (a UpstreamAuthType) IsValid() bool {
	return a == UpstreamAuthNone || a == UpstreamAuthBasic || a == UpstreamAuthToken
}

// PackageRegistryUpstream describes a remote registry that an owner's package registry can
// proxy / cache for a given package Type. Shared across formats (maven first, then container, ...).
type PackageRegistryUpstream struct {
	ID      int64  `xorm:"pk autoincr"`
	OwnerID int64  `xorm:"UNIQUE(s) INDEX NOT NULL"`
	Type    Type   `xorm:"UNIQUE(s) INDEX NOT NULL"`
	Name    string `xorm:"UNIQUE(s) NOT NULL"`
	URL     string `xorm:"NOT NULL"`

	Mode         UpstreamMode     `xorm:"NOT NULL DEFAULT 'pull_through'"`
	AuthType     UpstreamAuthType `xorm:"NOT NULL DEFAULT 'none'"`
	AuthUsername string           `xorm:"NOT NULL DEFAULT ''"`
	// AuthSecret is PoC-only plaintext. TODO: store via the secret mechanism before upstreaming.
	AuthSecret string `xorm:"NOT NULL DEFAULT ''"`

	// MetadataTTL is the revalidation window (seconds) for mutable content in pull_through mode.
	MetadataTTL int64 `xorm:"NOT NULL DEFAULT 900"`
	Enabled     bool  `xorm:"INDEX NOT NULL DEFAULT true"`

	// Observability: FetchCount = upstream fetches, HitCount = served from local cache.
	FetchCount int64 `xorm:"NOT NULL DEFAULT 0"`
	HitCount   int64 `xorm:"NOT NULL DEFAULT 0"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL DEFAULT 0"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL DEFAULT 0"`
}

func InsertUpstream(ctx context.Context, u *PackageRegistryUpstream) (*PackageRegistryUpstream, error) {
	return u, db.Insert(ctx, u)
}

func GetUpstreamByID(ctx context.Context, id int64) (*PackageRegistryUpstream, error) {
	u := &PackageRegistryUpstream{}
	has, err := db.GetEngine(ctx).ID(id).Get(u)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, ErrPackageRegistryUpstreamNotExist
	}
	return u, nil
}

func GetUpstreamsByOwner(ctx context.Context, ownerID int64) ([]*PackageRegistryUpstream, error) {
	ups := make([]*PackageRegistryUpstream, 0, 10)
	return ups, db.GetEngine(ctx).Where("owner_id = ?", ownerID).OrderBy("id ASC").Find(&ups)
}

// GetEnabledUpstreamByOwnerAndType returns the first enabled upstream for an (owner, type).
// MVP: a single active upstream per (owner, type) is used by the proxy hook.
func GetEnabledUpstreamByOwnerAndType(ctx context.Context, ownerID int64, packageType Type) (*PackageRegistryUpstream, error) {
	u := &PackageRegistryUpstream{}
	has, err := db.GetEngine(ctx).
		Where("owner_id = ? AND type = ? AND enabled = ?", ownerID, packageType, true).
		OrderBy("id ASC").
		Get(u)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, ErrPackageRegistryUpstreamNotExist
	}
	return u, nil
}

func UpdateUpstream(ctx context.Context, u *PackageRegistryUpstream) error {
	_, err := db.GetEngine(ctx).ID(u.ID).AllCols().Update(u)
	return err
}

func DeleteUpstreamByID(ctx context.Context, id int64) error {
	_, err := db.GetEngine(ctx).ID(id).Delete(&PackageRegistryUpstream{})
	return err
}

// IncrUpstreamFetchCount atomically increments the upstream-fetch counter (observability).
func IncrUpstreamFetchCount(ctx context.Context, id int64) error {
	_, err := db.GetEngine(ctx).ID(id).Incr("fetch_count").Update(new(PackageRegistryUpstream))
	return err
}

// IncrUpstreamHitCount atomically increments the served-from-cache counter (observability).
func IncrUpstreamHitCount(ctx context.Context, id int64) error {
	_, err := db.GetEngine(ctx).ID(id).Incr("hit_count").Update(new(PackageRegistryUpstream))
	return err
}
