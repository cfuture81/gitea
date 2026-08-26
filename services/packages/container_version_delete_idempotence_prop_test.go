// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package packages

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"gitea.dev/models/db"
	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	container_module "gitea.dev/modules/packages/container"
	"gitea.dev/modules/setting"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
	"xorm.io/builder"
)

// p10 prefixes every helper in this file. Sibling property tests share the services/packages test
// binary, so identifiers here are deliberately namespaced to the property they belong to.

// p10Seq makes every generated container graph land in its own package, so one iteration can never
// see or delete another iteration's rows.
var p10Seq atomic.Int64

// p10Shape is the generated shape of one container version graph. It varies exactly the axes that
// change which branches RemoveProxiedVersionAndOrphans takes:
//
//   - ArchCount 0 means a plain manifest with no fan-out, 1 a single-arch index, >1 a multi-arch
//     index whose children are cascaded into.
//   - SharedChildren decides how many of those children a second, retained tag also references, so
//     the cascade has to keep some children and delete others. It is clamped to ArchCount.
//   - ExtraTags adds unrelated tagged versions (each with an exclusive child of its own) that must
//     survive both deletes untouched.
//   - SimulatePackageGC only takes effect when the first delete empties the package: it drops the
//     now-childless package row the way the cleanup_packages cron eventually would, so the second
//     delete runs against a package that no longer exists at all.
type p10Shape struct {
	ArchCount         int
	SharedChildren    int
	ExtraTags         int
	SimulatePackageGC bool
}

// p10GenShape composes the graph generator from gopter's gen combinators only.
func p10GenShape() gopter.Gen {
	return gen.Struct(reflect.TypeOf(p10Shape{}), map[string]gopter.Gen{
		"ArchCount":      gen.IntRange(0, 4),
		"SharedChildren": gen.IntRange(0, 4),
		// Weighted towards 0 so the "package becomes empty" edge case is hit often.
		"ExtraTags":         gen.Frequency(map[int]gopter.Gen{3: gen.Const(0), 2: gen.IntRange(1, 2)}),
		"SimulatePackageGC": gen.Bool(),
	})
}

// p10Digest builds a syntactically valid, collision-free manifest digest.
func p10Digest(seq int64, slot int) string {
	sum := sha512.Sum512([]byte(fmt.Sprintf("p10-digest-%d-%d", seq, slot)))
	return "sha256:" + hex.EncodeToString(sum[:])[:64]
}

// p10Blob inserts (or reuses) a blob whose four hash columns are derived from key, which keeps the
// fixed column widths valid while staying unique per key.
func p10Blob(ctx context.Context, key string) (*packages_model.PackageBlob, error) {
	sum := sha512.Sum512([]byte(key))
	h := hex.EncodeToString(sum[:])
	pb, _, err := packages_model.GetOrInsertBlob(ctx, &packages_model.PackageBlob{
		Size:       int64(len(key)),
		HashMD5:    h[:32],
		HashSHA1:   h[:40],
		HashSHA256: h[:64],
		HashSHA512: h,
	})
	return pb, err
}

// p10InsertVersion creates one container version with the property and file rows a real one has:
// the manifest-tagged marker for tags, one manifest.reference property per child digest, proxy
// provenance markers, its own manifest file and a layer file pointing at the package's shared
// layer blob.
func p10InsertVersion(ctx context.Context, p *packages_model.Package, creatorID int64, version string, tagged bool, refs []string, layer *packages_model.PackageBlob) (*packages_model.PackageVersion, error) {
	pv, err := packages_model.GetOrInsertVersion(ctx, &packages_model.PackageVersion{
		PackageID:    p.ID,
		CreatorID:    creatorID,
		Version:      version,
		LowerVersion: strings.ToLower(version),
		MetadataJSON: "{}",
	})
	if err != nil {
		return nil, err
	}

	if tagged {
		// Production stores an empty value for this marker (see storeManifest).
		if _, err := packages_model.InsertProperty(ctx, packages_model.PropertyTypeVersion, pv.ID, container_module.PropertyManifestTagged, ""); err != nil {
			return nil, err
		}
	}
	for _, ref := range refs {
		if _, err := packages_model.InsertProperty(ctx, packages_model.PropertyTypeVersion, pv.ID, container_module.PropertyManifestReference, ref); err != nil {
			return nil, err
		}
	}
	// Proxy provenance, so the snapshot also covers the "upstream.*" properties a delete must drop.
	if _, err := packages_model.InsertProperty(ctx, packages_model.PropertyTypeVersion, pv.ID, packages_model.PropertyUpstreamCached, "1"); err != nil {
		return nil, err
	}
	if _, err := packages_model.InsertProperty(ctx, packages_model.PropertyTypeVersion, pv.ID, packages_model.PropertyUpstreamSource, "p10-upstream"); err != nil {
		return nil, err
	}

	manifestBlob, err := p10Blob(ctx, fmt.Sprintf("p10-manifest-%d-%s", p.ID, version))
	if err != nil {
		return nil, err
	}
	if _, err := packages_model.TryInsertFile(ctx, &packages_model.PackageFile{
		VersionID: pv.ID,
		BlobID:    manifestBlob.ID,
		Name:      container_module.ManifestFilename,
		LowerName: container_module.ManifestFilename,
		IsLead:    true,
	}); err != nil {
		return nil, err
	}
	if _, err := packages_model.TryInsertFile(ctx, &packages_model.PackageFile{
		VersionID: pv.ID,
		BlobID:    layer.ID,
		Name:      "p10-layer",
		LowerName: "p10-layer",
	}); err != nil {
		return nil, err
	}
	return pv, nil
}

// p10BuildGraph materialises one generated shape and returns the package plus the version selected
// for deletion.
func p10BuildGraph(ctx context.Context, ownerID, creatorID int64, shape p10Shape) (*packages_model.Package, *packages_model.PackageVersion, error) {
	seq := p10Seq.Add(1)
	name := fmt.Sprintf("p10-img-%d", seq)

	p, err := packages_model.TryInsertPackage(ctx, &packages_model.Package{
		OwnerID:   ownerID,
		Type:      packages_model.TypeContainer,
		Name:      name,
		LowerName: name,
	})
	if err != nil {
		return nil, nil, err
	}

	// One layer blob shared by every version of the package, as real image graphs share layers.
	layer, err := p10Blob(ctx, fmt.Sprintf("p10-shared-layer-%d", seq))
	if err != nil {
		return nil, nil, err
	}

	children := make([]string, 0, shape.ArchCount)
	for i := range shape.ArchCount {
		dgst := p10Digest(seq, i)
		if _, err := p10InsertVersion(ctx, p, creatorID, dgst, false, nil, layer); err != nil {
			return nil, nil, err
		}
		children = append(children, dgst)
	}

	target, err := p10InsertVersion(ctx, p, creatorID, fmt.Sprintf("tag-%d", seq), true, children, layer)
	if err != nil {
		return nil, nil, err
	}

	if shared := min(shape.SharedChildren, shape.ArchCount); shared > 0 {
		if _, err := p10InsertVersion(ctx, p, creatorID, fmt.Sprintf("keep-%d", seq), true, children[:shared], layer); err != nil {
			return nil, nil, err
		}
	}

	for j := range shape.ExtraTags {
		dgst := p10Digest(seq, 1000+j)
		if _, err := p10InsertVersion(ctx, p, creatorID, dgst, false, nil, layer); err != nil {
			return nil, nil, err
		}
		if _, err := p10InsertVersion(ctx, p, creatorID, fmt.Sprintf("other-%d-%d", seq, j), true, []string{dgst}, layer); err != nil {
			return nil, nil, err
		}
	}

	return p, target, nil
}

// p10Snapshot renders the state a second delete must not change.
//
// Two layers are captured:
//
//   - global row counts for every package table, which catch a leaked or extra row anywhere -
//     including orphaned property/file rows that a per-package query could not see, and collateral
//     damage to other packages;
//   - the full per-package detail: whether the package row still exists, and for every surviving
//     version its id/version/internal flag/metadata plus all of its version properties and all of
//     its files with the blob each points at.
func p10Snapshot(ctx context.Context, packageID int64) (string, error) {
	var lines []string

	for _, c := range []struct {
		table string
		bean  any
	}{
		{"package", new(packages_model.Package)},
		{"package_version", new(packages_model.PackageVersion)},
		{"package_property", new(packages_model.PackageProperty)},
		{"package_file", new(packages_model.PackageFile)},
		{"package_blob", new(packages_model.PackageBlob)},
	} {
		n, err := db.CountByBean(ctx, c.bean)
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("count %s=%d", c.table, n))
	}

	pkgExists, err := db.ExistByID[packages_model.Package](ctx, packageID)
	if err != nil {
		return "", err
	}
	lines = append(lines, fmt.Sprintf("package %d exists=%t", packageID, pkgExists))

	pvs := make([]*packages_model.PackageVersion, 0, 10)
	if err := db.GetEngine(ctx).Where("package_id = ?", packageID).OrderBy("id").Find(&pvs); err != nil {
		return "", err
	}

	for _, pv := range pvs {
		lines = append(lines, fmt.Sprintf("version id=%d version=%q internal=%t metadata=%q", pv.ID, pv.LowerVersion, pv.IsInternal, pv.MetadataJSON))

		pps, err := packages_model.GetProperties(ctx, packages_model.PropertyTypeVersion, pv.ID)
		if err != nil {
			return "", err
		}
		props := make([]string, 0, len(pps))
		for _, pp := range pps {
			props = append(props, fmt.Sprintf("  property version=%d %s=%q", pv.ID, pp.Name, pp.Value))
		}
		slices.Sort(props)
		lines = append(lines, props...)

		pfs, err := packages_model.GetFilesByVersionID(ctx, pv.ID)
		if err != nil {
			return "", err
		}
		files := make([]string, 0, len(pfs))
		for _, pf := range pfs {
			files = append(files, fmt.Sprintf("  file version=%d id=%d name=%q blob=%d lead=%t", pv.ID, pf.ID, pf.LowerName, pf.BlobID, pf.IsLead))
		}
		slices.Sort(files)
		lines = append(lines, files...)
	}

	return strings.Join(lines, "\n"), nil
}

// Feature: proxy-registry-admin-and-orgs, Property 10: Delete is idempotent
//
// For any package version, deleting it and then attempting to delete it again leaves the system in
// the same state as after the first deletion (the second attempt has no additional effect).
//
// "Same state" is compared through p10Snapshot: the global row counts of all five package tables
// plus, for the package under test, the surviving version rows with their version properties and
// their files. The second call must additionally return nil rather than a not-found error - an
// error would leave the rows unchanged but still be a behaviour regression, because C7 specifies
// the repeated delete as a no-op.
//
// The generated shapes cover single-arch and multi-arch targets, children shared with a retained
// tag versus children exclusive to the target, unrelated tags that must survive, and the case
// where the target is the package's only version - optionally with the empty package row dropped
// in between, so the second delete exercises the "whole package already gone" branch as well.
//
// **Validates: Requirements 8.1**
func TestContainerVersionDeleteIsIdempotent(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	// RemovePackageVersion consults the specialization registry, which panics while empty; the
	// container type has no specialization of its own, so the default behaviour is registered.
	GetSpecManager().Add(packages_model.TypeContainer, &specDefault{})

	// Exercise the negative-cache clearing branch too, which is skipped while the proxy feature is
	// off. Restored afterwards so the setting stays at its default for other tests in this binary.
	defer func(orig bool) { setting.Packages.EnableUpstreamProxy = orig }(setting.Packages.EnableUpstreamProxy)
	setting.Packages.EnableUpstreamProxy = true

	doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 3})

	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100

	properties := gopter.NewProperties(params)

	properties.Property("deleting an already-deleted version is a no-op", prop.ForAll(
		func(shape p10Shape) string {
			p, target, err := p10BuildGraph(ctx, owner.ID, doer.ID, shape)
			if err != nil {
				return fmt.Sprintf("build graph %+v: %v", shape, err)
			}

			if err := RemoveProxiedVersionAndOrphans(ctx, doer, target); err != nil {
				return fmt.Sprintf("first delete of version %d: %v", target.ID, err)
			}

			// Emulate the cleanup cron collecting a package that lost its last version, so the
			// repeated delete also has to cope with the package row being gone.
			if shape.SimulatePackageGC {
				remaining, err := db.Exist[packages_model.PackageVersion](ctx, builder.Eq{"package_id": p.ID})
				if err != nil {
					return fmt.Sprintf("probe remaining versions of package %d: %v", p.ID, err)
				}
				if !remaining {
					if err := packages_model.DeletePackageByID(ctx, p.ID); err != nil {
						return fmt.Sprintf("simulated GC of package %d: %v", p.ID, err)
					}
				}
			}

			before, err := p10Snapshot(ctx, p.ID)
			if err != nil {
				return fmt.Sprintf("snapshot after first delete: %v", err)
			}

			if err := RemoveProxiedVersionAndOrphans(ctx, doer, target); err != nil {
				return fmt.Sprintf("second delete of version %d returned an error instead of being a no-op: %v", target.ID, err)
			}

			after, err := p10Snapshot(ctx, p.ID)
			if err != nil {
				return fmt.Sprintf("snapshot after second delete: %v", err)
			}

			if before != after {
				return fmt.Sprintf("second delete changed the state for %+v\n--- after first delete ---\n%s\n--- after second delete ---\n%s", shape, before, after)
			}
			return ""
		},
		p10GenShape(),
	))

	properties.TestingRun(t)
}
