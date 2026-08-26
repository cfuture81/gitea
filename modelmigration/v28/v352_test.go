// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"testing"

	"gitea.dev/modelmigration/migrationtest"
	"gitea.dev/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// packageRegistryUpstreamBeforeV352 mirrors the package_registry_upstream table as it existed
// before the migration: every column of the current model except target_owner_id, remote_prefix
// and is_admin_managed. pinned_tags is declared plain TEXT (the same column type xorm derives from
// `JSON TEXT`) so the fixture can store and compare the serialized JSON verbatim.
type packageRegistryUpstreamBeforeV352 struct {
	ID      int64  `xorm:"pk autoincr"`
	OwnerID int64  `xorm:"UNIQUE(s) INDEX NOT NULL"`
	Type    string `xorm:"UNIQUE(s) INDEX NOT NULL"`
	Name    string `xorm:"UNIQUE(s) NOT NULL"`
	URL     string `xorm:"NOT NULL"`

	Mode         string `xorm:"NOT NULL DEFAULT 'pull_through'"`
	AuthType     string `xorm:"NOT NULL DEFAULT 'none'"`
	AuthUsername string `xorm:"NOT NULL DEFAULT ''"`
	AuthSecret   string `xorm:"NOT NULL DEFAULT ''"`

	MetadataTTL int64 `xorm:"NOT NULL DEFAULT 900"`
	Priority    int64 `xorm:"NOT NULL DEFAULT 100"`
	Enabled     bool  `xorm:"INDEX NOT NULL DEFAULT true"`

	FetchCount int64 `xorm:"NOT NULL DEFAULT 0"`
	HitCount   int64 `xorm:"NOT NULL DEFAULT 0"`

	PinnedTags string `xorm:"TEXT"`

	CreatedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
	UpdatedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (packageRegistryUpstreamBeforeV352) TableName() string { return "package_registry_upstream" }

// packageRegistryUpstreamAfterV352 is the post-migration read shape: the pre-migration columns
// plus the three added ones, so a single query can assert both the backfill and that nothing else
// moved.
type packageRegistryUpstreamAfterV352 struct {
	ID      int64
	OwnerID int64
	Type    string
	Name    string
	URL     string

	Mode         string
	AuthType     string
	AuthUsername string
	AuthSecret   string

	MetadataTTL int64
	Priority    int64
	Enabled     bool

	FetchCount int64
	HitCount   int64

	PinnedTags string

	CreatedUnix timeutil.TimeStamp
	UpdatedUnix timeutil.TimeStamp

	TargetOwnerID  int64
	RemotePrefix   string
	IsAdminManaged bool
}

const selectUpstreamRowsV352 = `SELECT id, owner_id, type, name, url,
	mode, auth_type, auth_username, auth_secret,
	metadata_ttl, priority, enabled,
	fetch_count, hit_count, pinned_tags,
	created_unix, updated_unix,
	target_owner_id, remote_prefix, is_admin_managed
	FROM package_registry_upstream ORDER BY id`

func TestAddTargetOwnerAndRemotePrefixToPackageRegistryUpstream(t *testing.T) {
	x, deferable := migrationtest.PrepareTestEnv(t, 0, new(packageRegistryUpstreamBeforeV352))
	defer deferable()
	if x == nil || t.Failed() {
		return
	}

	// Pre-existing owner-scoped configuration. Every owner_id is deliberately non-zero so the
	// target_owner_id backfill is observable, and the remaining columns carry non-default values so
	// an accidental rewrite would show up.
	before := []*packageRegistryUpstreamBeforeV352{
		{
			OwnerID: 5, Type: "container", Name: "dockerhub", URL: "https://registry-1.docker.io",
			Mode: "pull_through", AuthType: "basic", AuthUsername: "robot", AuthSecret: "s3cret",
			MetadataTTL: 900, Priority: 100, Enabled: true,
			FetchCount: 7, HitCount: 42,
			PinnedTags:  `[{"image":"library/mongo","tag":"latest","target":"4.4.29"}]`,
			CreatedUnix: 1700000000, UpdatedUnix: 1700000001,
		},
		{
			// Same owner and type as the first row: the unique key is (owner_id, type, name), and
			// several upstreams in one resolution group must all be backfilled.
			OwnerID: 5, Type: "container", Name: "quay", URL: "https://quay.io",
			Mode: "frozen", AuthType: "token", AuthUsername: "", AuthSecret: "tok",
			MetadataTTL: 60, Priority: 200, Enabled: false,
			FetchCount: 0, HitCount: 3,
			PinnedTags:  "",
			CreatedUnix: 1700000002, UpdatedUnix: 1700000003,
		},
		{
			OwnerID: 9, Type: "maven", Name: "central", URL: "https://repo1.maven.org/maven2",
			Mode: "pull_through", AuthType: "none", AuthUsername: "", AuthSecret: "",
			MetadataTTL: 300, Priority: 5, Enabled: true,
			FetchCount: 11, HitCount: 0,
			PinnedTags:  "",
			CreatedUnix: 1700000004, UpdatedUnix: 1700000005,
		},
	}
	for _, row := range before {
		_, err := x.Insert(row)
		require.NoError(t, err)
	}

	require.NoError(t, AddTargetOwnerAndRemotePrefixToPackageRegistryUpstream(t.Context(), x))

	var rows []packageRegistryUpstreamAfterV352
	require.NoError(t, x.SQL(selectUpstreamRowsV352).Find(&rows))
	require.Len(t, rows, len(before), "the migration must neither drop nor add rows")

	for i, got := range rows {
		want := before[i]

		// The backfill: pre-existing owner-scoped rows receive their own owner as target owner.
		assert.NotZero(t, got.OwnerID, "fixture owner_id must be non-zero for the assertion to bite")
		assert.Equal(t, got.OwnerID, got.TargetOwnerID, "row %d: target_owner_id must be backfilled from owner_id", got.ID)

		// The two other new columns keep their declared defaults where nothing backfills them.
		assert.Empty(t, got.RemotePrefix, "row %d: remote_prefix must default to the empty string", got.ID)
		assert.False(t, got.IsAdminManaged, "row %d: is_admin_managed must default to false", got.ID)

		// Every pre-existing column is untouched (Requirement 3.3: no field dropped or rewritten),
		// including the timestamps - the raw backfill must not bump updated_unix.
		assert.Equal(t, want.OwnerID, got.OwnerID, "row %d: owner_id", got.ID)
		assert.Equal(t, want.Type, got.Type, "row %d: type", got.ID)
		assert.Equal(t, want.Name, got.Name, "row %d: name", got.ID)
		assert.Equal(t, want.URL, got.URL, "row %d: url", got.ID)
		assert.Equal(t, want.Mode, got.Mode, "row %d: mode", got.ID)
		assert.Equal(t, want.AuthType, got.AuthType, "row %d: auth_type", got.ID)
		assert.Equal(t, want.AuthUsername, got.AuthUsername, "row %d: auth_username", got.ID)
		assert.Equal(t, want.AuthSecret, got.AuthSecret, "row %d: auth_secret", got.ID)
		assert.Equal(t, want.MetadataTTL, got.MetadataTTL, "row %d: metadata_ttl", got.ID)
		assert.Equal(t, want.Priority, got.Priority, "row %d: priority", got.ID)
		assert.Equal(t, want.Enabled, got.Enabled, "row %d: enabled", got.ID)
		assert.Equal(t, want.FetchCount, got.FetchCount, "row %d: fetch_count", got.ID)
		assert.Equal(t, want.HitCount, got.HitCount, "row %d: hit_count", got.ID)
		assert.Equal(t, want.PinnedTags, got.PinnedTags, "row %d: pinned_tags", got.ID)
		assert.Equal(t, want.CreatedUnix, got.CreatedUnix, "row %d: created_unix", got.ID)
		assert.Equal(t, want.UpdatedUnix, got.UpdatedUnix, "row %d: updated_unix", got.ID)
	}

	// Idempotence: a second application leaves the table in exactly the same state.
	require.NoError(t, AddTargetOwnerAndRemotePrefixToPackageRegistryUpstream(t.Context(), x))
	var afterSecondRun []packageRegistryUpstreamAfterV352
	require.NoError(t, x.SQL(selectUpstreamRowsV352).Find(&afterSecondRun))
	assert.Equal(t, rows, afterSecondRun, "re-running the migration must not change any row")

	// An explicitly set target owner is never rewritten: the backfill is scoped to rows still at
	// the column default, so a row whose target owner already diverges from owner_id survives.
	_, err := x.Exec("UPDATE package_registry_upstream SET target_owner_id = ? WHERE id = ?", 77, rows[2].ID)
	require.NoError(t, err)
	require.NoError(t, AddTargetOwnerAndRemotePrefixToPackageRegistryUpstream(t.Context(), x))
	var targetOwnerID int64
	has, err := x.SQL("SELECT target_owner_id FROM package_registry_upstream WHERE id = ?", rows[2].ID).Get(&targetOwnerID)
	require.NoError(t, err)
	require.True(t, has)
	assert.EqualValues(t, 77, targetOwnerID, "an explicitly set target_owner_id must not be backfilled over")
}
