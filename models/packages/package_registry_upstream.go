// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package packages

import (
	"context"
	"strings"

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

// UpstreamPin aliases a mutable tag to a fixed upstream reference for one image, so internal
// clients keep requesting the mutable tag (e.g. "latest") but are served the pinned version's
// content. This is the "freeze a specific version" control (container MVP).
type UpstreamPin struct {
	Image  string `json:"image"`  // e.g. "library/mongo" or "noenv/mongo"
	Tag    string `json:"tag"`    // tag the client requests, e.g. "latest"
	Target string `json:"target"` // upstream reference actually fetched, e.g. "4.4.29"
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
	// Priority orders upstreams within an (owner,type) group; lower is tried first (ties by id).
	Priority int64 `xorm:"NOT NULL DEFAULT 100"`
	Enabled  bool  `xorm:"INDEX NOT NULL DEFAULT true"`

	// TargetOwnerID is the owner/org under which packages fetched through this upstream are
	// created and cached. It is the admin-facing name for the existing OwnerID scoping column and
	// the invariant TargetOwnerID == OwnerID holds for every row (see normalizeTargetOwner), so
	// the resolver, the (OwnerID, Type, Name) unique key and all existing queries stay unchanged.
	TargetOwnerID int64 `xorm:"NOT NULL DEFAULT 0"`
	// RemotePrefix is a namespace path segment prepended to the image when building the upstream
	// request address (see UpstreamAddr). Empty means "request the image path unchanged".
	RemotePrefix string `xorm:"NOT NULL DEFAULT ''"`
	// IsAdminManaged records that this row was created/edited through the site admin panel.
	// Provenance only - it has no effect on upstream resolution.
	IsAdminManaged bool `xorm:"NOT NULL DEFAULT false"`

	// Observability: FetchCount = upstream fetches, HitCount = served from local cache.
	FetchCount int64 `xorm:"NOT NULL DEFAULT 0"`
	HitCount   int64 `xorm:"NOT NULL DEFAULT 0"`

	// PinnedTags aliases mutable tags to fixed upstream references (see UpstreamPin). JSON column.
	PinnedTags []*UpstreamPin `xorm:"JSON TEXT"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL DEFAULT 0"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL DEFAULT 0"`
}

// normalizeTargetOwner keeps the invariant TargetOwnerID == OwnerID so the two can never diverge.
// Callers may set either field: owner-scoped callers set OwnerID only, the admin panel selects a
// Target_Owner; whichever is populated wins, with OwnerID taking precedence when both are set.
func (u *PackageRegistryUpstream) normalizeTargetOwner() {
	if u.OwnerID == 0 && u.TargetOwnerID != 0 {
		u.OwnerID = u.TargetOwnerID
		return
	}
	u.TargetOwnerID = u.OwnerID
}

// UpstreamAddr returns the address used when addressing image on the upstream registry: image
// itself when no RemotePrefix is configured, otherwise the image below RemotePrefix. Pure helper -
// used to compose the upstream /v2/<addr>/... URL and the matching Bearer scope.
func (u *PackageRegistryUpstream) UpstreamAddr(image string) string {
	if u.RemotePrefix == "" {
		return image
	}
	return u.RemotePrefix + "/" + image
}

func InsertUpstream(ctx context.Context, u *PackageRegistryUpstream) (*PackageRegistryUpstream, error) {
	u.normalizeTargetOwner()
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

// GetAllUpstreams returns the upstreams of every owner, grouped by target owner and type and
// ordered within a group by resolution priority - the listing order for the site admin view.
// Ordering uses owner_id because it is the target owner column (TargetOwnerID == OwnerID), which
// also keeps the order stable for rows written before the target_owner_id backfill.
func GetAllUpstreams(ctx context.Context) ([]*PackageRegistryUpstream, error) {
	ups := make([]*PackageRegistryUpstream, 0, 10)
	return ups, db.GetEngine(ctx).OrderBy("owner_id ASC, type ASC, priority ASC, id ASC").Find(&ups)
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

// GetEnabledUpstreamsByOwnerAndType returns all enabled upstreams for an (owner, type), ordered by
// priority ascending then id (the resolution order for group/virtual behaviour: try each in turn
// on a local miss). Returns an empty slice when none are configured.
func GetEnabledUpstreamsByOwnerAndType(ctx context.Context, ownerID int64, packageType Type) ([]*PackageRegistryUpstream, error) {
	ups := make([]*PackageRegistryUpstream, 0, 4)
	err := db.GetEngine(ctx).
		Where("owner_id = ? AND type = ? AND enabled = ?", ownerID, packageType, true).
		OrderBy("priority ASC, id ASC").
		Find(&ups)
	return ups, err
}

func UpdateUpstream(ctx context.Context, u *PackageRegistryUpstream) error {
	u.normalizeTargetOwner()
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

// ResolvePin returns the target upstream reference for (image, requestedTag) if a pin exists for
// this upstream, or "" when none matches. Comparison is case-insensitive.
func (u *PackageRegistryUpstream) ResolvePin(image, tag string) string {
	for _, p := range u.PinnedTags {
		if p == nil {
			continue
		}
		if strings.EqualFold(p.Image, image) && strings.EqualFold(p.Tag, tag) {
			return p.Target
		}
	}
	return ""
}

// Package properties set on proxied-cached versions (observability + retention targeting).
const (
	PropertyUpstreamCached = "upstream.cached"
	PropertyUpstreamSource = "upstream.source"
)

// TagVersionCached marks a package version as fetched from an upstream proxy (source = upstream
// name), so cached content is distinguishable from first-party uploads in the UI/API and for
// retention reasoning. Best-effort: property errors are returned for the caller to log.
func TagVersionCached(ctx context.Context, versionID int64, source string) error {
	if err := InsertOrUpdateProperty(ctx, PropertyTypeVersion, versionID, PropertyUpstreamCached, "1"); err != nil {
		return err
	}
	return InsertOrUpdateProperty(ctx, PropertyTypeVersion, versionID, PropertyUpstreamSource, source)
}
