// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package packages

import (
	"context"
	"errors"
	"strconv"
	"strings"

	packages_model "gitea.dev/models/packages"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	container_module "gitea.dev/modules/packages/container"
	"gitea.dev/modules/packages/proxycache"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
)

// RemoveProxiedVersionAndOrphans removes a single package version, adding the container-specific
// handling a plain RemovePackageVersion lacks:
//
//  1. For a container version it cascades to the untagged per-arch manifest versions the removed
//     version referenced (its "container.manifest.reference" properties), deleting a child only
//     when no retained version still references it. Children shared with a retained version are
//     left intact, so no manifest list is left with a dangling reference.
//  2. It clears the proxy negative-cache entry of every removed reference, so the next pull of a
//     deleted proxied version re-enters the upstream fetch path instead of being suppressed for
//     the remainder of the upstream's MetadataTTL window.
//
// Version properties - including the proxy provenance markers "upstream.cached" /
// "upstream.source" - are removed with their version by RemovePackageVersion, which is why a
// re-pull re-creates the version as proxied rather than resurrecting it as internal.
//
// Blob storage is intentionally NOT touched here: package_blob rows are reference-counted through
// package_file, and the cleanup_packages cron removes a blob only once no package_file references
// it any more (see CleanupExpiredData / packages_model.FindExpiredUnreferencedBlobs). Deleting a
// version drops that version's package_file rows, so blobs shared with a retained version keep a
// live reference and survive, while blobs that became unreferenced are collected later.
//
// Non-container formats keep the standard RemovePackageVersion behaviour: their versions have no
// child-manifest fan-out and no proxy reference key of this shape.
//
// The operation is idempotent: deleting an already-deleted version is a no-op returning nil.
func RemoveProxiedVersionAndOrphans(ctx context.Context, doer *user_model.User, pv *packages_model.PackageVersion) error {
	if pv == nil {
		return nil
	}

	p, err := packages_model.GetPackageByID(ctx, pv.PackageID)
	if err != nil {
		if errors.Is(err, util.ErrNotExist) {
			return nil // the whole package is already gone
		}
		return err
	}

	if p.Type != packages_model.TypeContainer {
		return RemovePackageVersion(ctx, doer, pv)
	}

	// Idempotence: a repeated delete of the same version must not error.
	if _, err := packages_model.GetVersionByID(ctx, pv.ID); err != nil {
		if errors.Is(err, util.ErrNotExist) {
			return nil
		}
		return err
	}

	// The child digests have to be read BEFORE the deletion, because RemovePackageVersion drops
	// the version properties that hold them.
	children, err := childManifestDigests(ctx, pv.ID)
	if err != nil {
		return err
	}

	if err := RemovePackageVersion(ctx, doer, pv); err != nil {
		return err
	}
	clearUpstreamNegativeCache(ctx, p, pv.LowerVersion)

	return removeOrphanedChildManifests(ctx, doer, p, children)
}

// removeOrphanedChildManifests deletes the untagged manifest versions named by digests that no
// retained version references any more.
//
// It MUST run after the referring version has been deleted: the "is it still referenced?" probe
// counts every remaining version carrying a matching container.manifest.reference property, so the
// target's own reference must already be gone for its exclusive children to look orphaned. A child
// still referenced by any retained version - another tag, or an index version stored under its
// digest - is kept, which is a superset of the design's "no other tagged version references it"
// rule and cannot produce a dangling reference.
//
// Errors on individual children are collected rather than aborting, so one undeletable child does
// not leave the remaining ones orphaned.
func removeOrphanedChildManifests(ctx context.Context, doer *user_model.User, p *packages_model.Package, digests []string) error {
	queue := make([]string, 0, len(digests))
	queue = append(queue, digests...)
	seen := make(map[string]bool, len(digests))
	var errs []error

	for len(queue) > 0 {
		dgst := queue[0]
		queue = queue[1:]
		if dgst == "" || seen[dgst] {
			continue
		}
		seen[dgst] = true

		referenced, err := packages_model.ExistVersion(ctx, &packages_model.PackageSearchOptions{
			PackageID: p.ID,
			Properties: map[string]string{
				container_module.PropertyManifestReference: dgst,
			},
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if referenced {
			continue // a retained version still needs this manifest
		}

		child, err := packages_model.GetVersionByNameAndVersion(ctx, p.OwnerID, packages_model.TypeContainer, p.LowerName, dgst)
		if err != nil {
			if !errors.Is(err, util.ErrNotExist) {
				errs = append(errs, err)
			}
			continue // already gone - nothing to cascade into
		}

		tagged, err := isTaggedVersion(ctx, child.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if tagged {
			continue // a tag the operator did not select is never removed implicitly
		}

		// Read the grandchildren before deleting, for the same reason as above. Gitea's index
		// validation only admits image manifests as index children, so this is normally empty;
		// draining it keeps the cascade correct for any nested graph.
		grandChildren, err := childManifestDigests(ctx, child.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		if err := RemovePackageVersion(ctx, doer, child); err != nil {
			errs = append(errs, err)
			continue
		}
		clearUpstreamNegativeCache(ctx, p, child.LowerVersion)
		queue = append(queue, grandChildren...)
	}

	return errors.Join(errs...)
}

// childManifestDigests returns the digests a manifest-list version references.
func childManifestDigests(ctx context.Context, versionID int64) ([]string, error) {
	pps, err := packages_model.GetPropertiesByName(ctx, packages_model.PropertyTypeVersion, versionID, container_module.PropertyManifestReference)
	if err != nil {
		return nil, err
	}
	digests := make([]string, 0, len(pps))
	for _, pp := range pps {
		digests = append(digests, pp.Value)
	}
	return digests, nil
}

// isTaggedVersion reports whether a container version carries the manifest-tagged marker.
func isTaggedVersion(ctx context.Context, versionID int64) (bool, error) {
	pps, err := packages_model.GetPropertiesByName(ctx, packages_model.PropertyTypeVersion, versionID, container_module.PropertyManifestTagged)
	if err != nil {
		return false, err
	}
	return len(pps) > 0, nil
}

// clearUpstreamNegativeCache unblocks "<upstreamID>|<image>|<reference>" for every container
// upstream of the package owner, so a pull right after a delete is allowed to reach the upstream
// again. Best-effort and a no-op while the proxy feature is disabled, where no key can exist.
func clearUpstreamNegativeCache(ctx context.Context, p *packages_model.Package, reference string) {
	if !setting.Packages.EnableUpstreamProxy || reference == "" {
		return
	}
	ups, err := packages_model.GetUpstreamsByOwner(ctx, p.OwnerID)
	if err != nil {
		log.Error("container delete: list upstreams of owner %d: %v", p.OwnerID, err)
		return
	}
	for _, up := range ups {
		if up.Type != packages_model.TypeContainer {
			continue
		}
		// Key layout mirrors proxyEnsureManifest: the image segment is lower-cased.
		proxycache.Clear(strconv.FormatInt(up.ID, 10) + "|" + strings.ToLower(p.LowerName) + "|" + reference)
	}
}
