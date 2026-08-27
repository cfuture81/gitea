// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package user

import (
	"testing"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/require"
)

// TestDeleteUserRemovesPackageRegistryUpstreams asserts that deleting a user cascades to the
// fork's package_registry_upstream rows. Without the cascade the rows outlive their owner with a
// dangling owner_id, keeping the plaintext AuthSecret alive and cluttering the admin list.
func TestDeleteUserRemovesPackageRegistryUpstreams(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	// user8 owns no repositories and is not an organization member, so a plain (non-purge)
	// deletion succeeds. Neither owner gets a package, so the "still owns packages" guard
	// (ErrUserOwnPackages) cannot block the deletion.
	deleted := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 8})
	kept := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 9})

	for _, owner := range []*user_model.User{deleted, kept} {
		_, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
			OwnerID:      owner.ID,
			Type:         packages_model.TypeContainer,
			Name:         "docker-hub",
			URL:          "https://registry-1.docker.io",
			AuthType:     packages_model.UpstreamAuthBasic,
			AuthUsername: "robot",
			AuthSecret:   "plaintext-secret",
			Enabled:      true,
		})
		require.NoError(t, err)
	}
	unittest.AssertExistsAndLoadBean(t, &packages_model.PackageRegistryUpstream{OwnerID: deleted.ID})

	require.NoError(t, DeleteUser(t.Context(), deleted, false))

	unittest.AssertNotExistsBean(t, &packages_model.PackageRegistryUpstream{OwnerID: deleted.ID})
	// another owner's upstream must be untouched by the cascade
	unittest.AssertExistsAndLoadBean(t, &packages_model.PackageRegistryUpstream{OwnerID: kept.ID})
}
