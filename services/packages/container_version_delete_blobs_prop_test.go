// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitea.dev/models/db"
	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	container_module "gitea.dev/modules/packages/container"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// Identifiers in this file are prefixed prop9 because package-level test helpers are shared across
// every _test.go file of services/packages.

// prop9OwnerID is the fixture organization every generated container package is created under.
// prop9CreatorID is an existing fixture user, needed because RemovePackageVersion builds a
// PackageDescriptor - which loads owner and creator - before deleting.
const (
	prop9OwnerID   = int64(3)
	prop9CreatorID = int64(2)
)

// prop9Seq keeps every iteration's package name and blob hashes unique, so iterations cannot share
// rows: package_blob is deduplicated by hash across the whole table, and a collision would silently
// couple two iterations' reference counts.
var prop9Seq atomic.Int64

// prop9GraphSpec is the generated shape of one container manifest graph.
//
// The graph always contains a mandatory skeleton that keeps the property non-vacuous - a target
// tag, a retained tag, a child manifest exclusive to the target, a child manifest shared by both
// tags, and one layer blob referenced by *every* child - and the generated fields add variation on
// top of it:
//
//   - ExtraLayers          size of the additional shared layer pool
//   - ExclusiveLayerMask   which extra layers the target-exclusive child references
//   - SharedLayerMask      which extra layers the shared child references
//   - ChildLayerMasks      one entry per additional child manifest; the mask picks its extra layers
//   - TagChildMasks        one entry per additional retained tag; the mask picks its children
//   - TargetChildMask      which additional children the target tag also references
//   - RetainedChildMask    which additional children the mandatory retained tag also references
//
// A mask bit selecting an additional child that no retained tag keeps makes that child an orphan of
// the delete; selecting it from a retained tag as well keeps it alive. Both outcomes therefore
// occur across iterations, as do children referenced by no tag at all.
type prop9GraphSpec struct {
	ExtraLayers        int
	ExclusiveLayerMask int
	SharedLayerMask    int
	ChildLayerMasks    []int
	TagChildMasks      []int
	TargetChildMask    int
	RetainedChildMask  int
}

// prop9GenGraphSpec composes the graph generator from gopter's combinators only.
func prop9GenGraphSpec() gopter.Gen {
	return gen.Struct(reflect.TypeOf(prop9GraphSpec{}), map[string]gopter.Gen{
		"ExtraLayers":        gen.IntRange(0, 3),
		"ExclusiveLayerMask": gen.IntRange(0, 7),
		"SharedLayerMask":    gen.IntRange(0, 7),
		"ChildLayerMasks":    gen.SliceOf(gen.IntRange(0, 7)),
		"TagChildMasks":      gen.SliceOf(gen.IntRange(0, 7)),
		"TargetChildMask":    gen.IntRange(0, 7),
		"RetainedChildMask":  gen.IntRange(0, 7),
	})
}

// prop9MaskSelect returns the indices below n whose bit is set in mask.
func prop9MaskSelect(mask, n int) []int {
	out := make([]int, 0, n)
	for i := range n {
		if mask&(1<<i) != 0 {
			out = append(out, i)
		}
	}
	return out
}

// prop9BlobHashes derives the four hash columns of package_blob from a seed. Each column carries a
// UNIQUE index, so distinct seeds must differ in all of them; deriving every column from one
// SHA-256 of the seed guarantees that.
func prop9BlobHashes(seed string) (md5sum, sha1sum, sha256sum, sha512sum string) {
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:]) // 64 hex chars
	return h[:32], h[:40], h, h + h
}

// prop9Version is one created package_version plus the blobs its package_file rows point at.
type prop9Version struct {
	label   string // human-readable name used in failure messages
	pv      *packages_model.PackageVersion
	blobIDs []int64
}

// prop9Graph is the materialised manifest graph of a single iteration.
type prop9Graph struct {
	pkg           *packages_model.Package
	target        *prop9Version   // the version the property deletes
	versions      []*prop9Version // every created version, including the target
	expectDeleted map[int64]bool  // version IDs the delete is expected to remove
	allBlobIDs    map[int64]bool  // every blob created by this iteration
	commonLayerID int64           // the layer blob referenced by every child manifest
}

// insertBlob inserts one package_blob identified by a seed and records it on the graph.
func (g *prop9Graph) insertBlob(ctx context.Context, seed string) (*packages_model.PackageBlob, error) {
	md5sum, sha1sum, sha256sum, sha512sum := prop9BlobHashes(seed)
	pb, _, err := packages_model.GetOrInsertBlob(ctx, &packages_model.PackageBlob{
		Size:       int64(len(seed)) + 1,
		HashMD5:    md5sum,
		HashSHA1:   sha1sum,
		HashSHA256: sha256sum,
		HashSHA512: sha512sum,
	})
	if err != nil {
		return nil, err
	}
	g.allBlobIDs[pb.ID] = true
	return pb, nil
}

// addVersion creates a package_version plus the package_file rows referencing the given blobs.
// Passing the same blob for two different versions is exactly how blob sharing is produced: two
// package_file rows, one per version, carrying the same blob_id.
func (g *prop9Graph) addVersion(ctx context.Context, label, version string, manifestBlob *packages_model.PackageBlob, contentBlobs []*packages_model.PackageBlob) (*prop9Version, error) {
	pv, err := packages_model.GetOrInsertVersion(ctx, &packages_model.PackageVersion{
		PackageID:    g.pkg.ID,
		CreatorID:    prop9CreatorID,
		Version:      version,
		LowerVersion: strings.ToLower(version),
		MetadataJSON: "{}",
	})
	if err != nil {
		return nil, fmt.Errorf("insert version %s: %w", label, err)
	}

	gv := &prop9Version{label: label, pv: pv}

	// The manifest itself, stored the way the container registry stores it.
	if _, err := packages_model.TryInsertFile(ctx, &packages_model.PackageFile{
		VersionID:    pv.ID,
		BlobID:       manifestBlob.ID,
		Name:         container_module.ManifestFilename,
		LowerName:    container_module.ManifestFilename,
		CompositeKey: "sha256:" + manifestBlob.HashSHA256,
		IsLead:       true,
	}); err != nil {
		return nil, fmt.Errorf("insert manifest file of %s: %w", label, err)
	}
	gv.blobIDs = append(gv.blobIDs, manifestBlob.ID)

	// Config and layer blobs, named after their digest like createFileFromBlobReference does.
	for _, pb := range contentBlobs {
		name := "sha256_" + pb.HashSHA256
		if _, err := packages_model.TryInsertFile(ctx, &packages_model.PackageFile{
			VersionID:    pv.ID,
			BlobID:       pb.ID,
			Name:         name,
			LowerName:    name,
			CompositeKey: "sha256:" + pb.HashSHA256,
		}); err != nil {
			return nil, fmt.Errorf("insert content file of %s: %w", label, err)
		}
		gv.blobIDs = append(gv.blobIDs, pb.ID)
	}

	g.versions = append(g.versions, gv)
	return gv, nil
}

// prop9BuildGraph materialises the generated graph in the database.
func prop9BuildGraph(ctx context.Context, spec prop9GraphSpec) (*prop9Graph, error) {
	iter := prop9Seq.Add(1)
	seed := func(parts ...any) string {
		return fmt.Sprintf("prop9-%d-%v", iter, parts)
	}

	pkgName := fmt.Sprintf("prop-shared-blobs-%d", iter)
	pkg, err := packages_model.TryInsertPackage(ctx, &packages_model.Package{
		OwnerID:   prop9OwnerID,
		Type:      packages_model.TypeContainer,
		Name:      pkgName,
		LowerName: pkgName,
	})
	if err != nil {
		return nil, fmt.Errorf("insert package: %w", err)
	}

	g := &prop9Graph{pkg: pkg, allBlobIDs: map[int64]bool{}, expectDeleted: map[int64]bool{}}

	// The layer pool. Index 0 is the layer every child manifest references, which is what turns
	// this from "does a delete keep unrelated rows" into "does a delete keep a blob whose other
	// referrer just disappeared".
	//
	// The extra layers are created lazily, on first selection by a child: creating the whole pool
	// eagerly would insert blobs that no package_file ever references - unreferenced from birth, a
	// state the container registry never produces - and those would look cron-collectable although
	// the delete never stranded them.
	commonLayer, err := g.insertBlob(ctx, seed("layer", 0))
	if err != nil {
		return nil, fmt.Errorf("insert common layer blob: %w", err)
	}
	g.commonLayerID = commonLayer.ID

	extraLayers := make(map[int]*packages_model.PackageBlob, spec.ExtraLayers)
	extraLayer := func(i int) (*packages_model.PackageBlob, error) {
		if pb, ok := extraLayers[i]; ok {
			return pb, nil
		}
		pb, err := g.insertBlob(ctx, seed("layer", i+1))
		if err != nil {
			return nil, fmt.Errorf("insert layer blob %d: %w", i+1, err)
		}
		extraLayers[i] = pb
		return pb, nil
	}

	// Child manifests: the two mandatory ones plus the generated extras. Every child gets a private
	// config blob and a private manifest blob (so a deleted child always strands something), the
	// common layer, and the extra layers its mask selects.
	childLayerMasks := append([]int{spec.ExclusiveLayerMask, spec.SharedLayerMask}, spec.ChildLayerMasks...)
	children := make([]*prop9Version, 0, len(childLayerMasks))
	for i, mask := range childLayerMasks {
		manifestBlob, err := g.insertBlob(ctx, seed("child-manifest", i))
		if err != nil {
			return nil, fmt.Errorf("insert child manifest blob %d: %w", i, err)
		}
		configBlob, err := g.insertBlob(ctx, seed("child-config", i))
		if err != nil {
			return nil, fmt.Errorf("insert child config blob %d: %w", i, err)
		}

		content := []*packages_model.PackageBlob{configBlob, commonLayer}
		for _, li := range prop9MaskSelect(mask, spec.ExtraLayers) {
			pb, err := extraLayer(li)
			if err != nil {
				return nil, err
			}
			content = append(content, pb)
		}

		// An untagged per-arch version is named by its own manifest digest.
		child, err := g.addVersion(ctx, fmt.Sprintf("child-%d", i), "sha256:"+manifestBlob.HashSHA256, manifestBlob, content)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}

	extraChildCount := len(spec.ChildLayerMasks)

	// Which children each tag references. Index 0 is the target tag, index 1 the mandatory retained
	// tag; both always reference the shared child (children[1]), and only the target references
	// children[0].
	tagChildren := make([][]int, 0, 2+len(spec.TagChildMasks))

	targetSet := []int{0, 1}
	for _, ci := range prop9MaskSelect(spec.TargetChildMask, extraChildCount) {
		targetSet = append(targetSet, ci+2)
	}
	tagChildren = append(tagChildren, targetSet)

	retainedSet := []int{1}
	for _, ci := range prop9MaskSelect(spec.RetainedChildMask, extraChildCount) {
		retainedSet = append(retainedSet, ci+2)
	}
	tagChildren = append(tagChildren, retainedSet)

	for _, mask := range spec.TagChildMasks {
		set := make([]int, 0, extraChildCount)
		for _, ci := range prop9MaskSelect(mask, extraChildCount) {
			set = append(set, ci+2)
		}
		tagChildren = append(tagChildren, set)
	}

	for i, childIdx := range tagChildren {
		manifestBlob, err := g.insertBlob(ctx, seed("index-manifest", i))
		if err != nil {
			return nil, fmt.Errorf("insert index manifest blob %d: %w", i, err)
		}
		tagName := fmt.Sprintf("tag-%d", i)
		tag, err := g.addVersion(ctx, tagName, tagName, manifestBlob, nil)
		if err != nil {
			return nil, err
		}
		if _, err := packages_model.InsertProperty(ctx, packages_model.PropertyTypeVersion, tag.pv.ID, container_module.PropertyManifestTagged, ""); err != nil {
			return nil, fmt.Errorf("mark %s tagged: %w", tag.label, err)
		}
		for _, ci := range childIdx {
			if _, err := packages_model.InsertProperty(ctx, packages_model.PropertyTypeVersion, tag.pv.ID, container_module.PropertyManifestReference, children[ci].pv.LowerVersion); err != nil {
				return nil, fmt.Errorf("reference child %d from %s: %w", ci, tag.label, err)
			}
		}
		if i == 0 {
			g.target = tag
			g.expectDeleted[tag.pv.ID] = true
		}
	}

	// A child of the target is expected to go only when no other tag still references it - the same
	// predicate removeOrphanedChildManifests applies, restated over the generated graph.
	for _, ci := range tagChildren[0] {
		referencedElsewhere := false
		for ti := 1; ti < len(tagChildren); ti++ {
			for _, other := range tagChildren[ti] {
				if other == ci {
					referencedElsewhere = true
				}
			}
		}
		if !referencedElsewhere {
			g.expectDeleted[children[ci].pv.ID] = true
		}
	}

	return g, nil
}

// prop9CountFilesReferencingBlob reports how many package_file rows point at a blob. This is the
// join the whole property turns on: it is what FindExpiredUnreferencedBlobs evaluates, and it is
// the only thing keeping a blob alive - package_blob has no reference-count column.
func prop9CountFilesReferencingBlob(ctx context.Context, blobID int64) (int64, error) {
	return db.GetEngine(ctx).Where("blob_id = ?", blobID).Count(new(packages_model.PackageFile))
}

func prop9VersionExists(ctx context.Context, versionID int64) (bool, error) {
	_, err := packages_model.GetVersionByID(ctx, versionID)
	if err != nil {
		if err == packages_model.ErrPackageNotExist {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func prop9BlobExists(ctx context.Context, blobID int64) (bool, error) {
	_, err := packages_model.GetBlobByID(ctx, blobID)
	if err != nil {
		if err == packages_model.ErrPackageBlobNotExist {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func prop9SortedIDs(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Feature: proxy-registry-admin-and-orgs, Property 9: Shared blobs are preserved for retained versions
//
// For any version whose blobs are shared with other retained versions, deleting that version
// removes the deleted version's manifest and metadata while preserving every blob still referenced
// by a retained version.
//
// # WHAT THIS ASSERTS, AND WHY IT IS PHRASED THIS WAY
//
// RemoveProxiedVersionAndOrphans contains no blob code at all, and neither does the
// DeletePackageVersionAndReferences it delegates to: that function drops the version's
// package_file rows and never touches package_blob - upstream marks the omission explicitly with
// "HINT: PACKAGE-DEFER-STORAGE-DELETE". Blob lifetime is therefore decided entirely by a join:
// FindExpiredUnreferencedBlobs LEFT JOINs package_file onto package_blob and selects the rows where
// no file matches and the blob is older than the cleanup cron's threshold. There is no
// reference-count column anywhere.
//
// Two consequences shape the assertions:
//
//  1. Preservation is asserted as "the blob row is present AND at least one package_file row still
//     references it". Presence alone would be too weak, because an unreferenced blob is also still
//     present right after a delete; the live reference is what actually guarantees the retained
//     version can still be served.
//  2. Orphaning is NOT asserted as immediate deletion, because in this codebase it is not
//     immediate - removal is deferred to the cleanup_packages cron. Asserting a synchronous delete
//     would assert something false about the code. The handoff to the cron is pinned instead: the
//     blobs that lost their last reference must still exist, and among this iteration's blobs they
//     must be exactly the set FindExpiredUnreferencedBlobs selects.
//
// Both halves are guarded against passing vacuously: the graph is checked for genuine sharing
// before the delete (a common layer blob with more than one referencing file, referenced from both
// a to-be-deleted and a to-be-retained version) and for genuine orphaning after it.
//
// **Validates: Requirements 8.5**
func TestSharedBlobsArePreservedForRetainedVersions(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	// RemovePackageVersion asks the specialization manager for the package type's hooks, and the
	// manager panics while its map is empty. Container has no specialization in production either,
	// so registering the default behaviour is faithful.
	GetSpecManager().Add(packages_model.TypeContainer, &specDefault{})

	doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: prop9CreatorID})

	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100
	// Bounds the generated slice lengths (extra children / extra tags) so 100 graphs stay quick.
	params.MaxSize = 3

	properties := gopter.NewProperties(params)

	properties.Property("deleting a version keeps every blob a retained version still references", prop.ForAll(
		func(spec prop9GraphSpec) string {
			g, err := prop9BuildGraph(ctx, spec)
			if err != nil {
				return err.Error()
			}

			var problems []string
			fail := func(format string, args ...any) {
				problems = append(problems, fmt.Sprintf(format, args...))
			}

			// Non-vacuity, part 1: the common layer really is shared, and it is shared across the
			// delete boundary - one of its referrers dies, another survives.
			sharedRefs, err := prop9CountFilesReferencingBlob(ctx, g.commonLayerID)
			if err != nil {
				return fmt.Sprintf("count files of common layer: %v", err)
			}
			if sharedRefs < 2 {
				fail("setup is vacuous: common layer blob %d has %d referencing files, want >= 2", g.commonLayerID, sharedRefs)
			}
			sharedFromDeleted, sharedFromRetained := 0, 0
			for _, gv := range g.versions {
				for _, id := range gv.blobIDs {
					if id != g.commonLayerID {
						continue
					}
					if g.expectDeleted[gv.pv.ID] {
						sharedFromDeleted++
					} else {
						sharedFromRetained++
					}
				}
			}
			if sharedFromDeleted == 0 || sharedFromRetained == 0 {
				fail("setup is vacuous: common layer blob %d referenced by %d deleted and %d retained versions, want both > 0",
					g.commonLayerID, sharedFromDeleted, sharedFromRetained)
			}

			// Expected blob sets, computed before the delete from the built graph.
			retainedBlobs := map[int64]bool{}
			deletedVersionBlobs := map[int64]bool{}
			for _, gv := range g.versions {
				target := retainedBlobs
				if g.expectDeleted[gv.pv.ID] {
					target = deletedVersionBlobs
				}
				for _, id := range gv.blobIDs {
					target[id] = true
				}
			}
			newlyOrphaned := map[int64]bool{}
			for id := range deletedVersionBlobs {
				if !retainedBlobs[id] {
					newlyOrphaned[id] = true
				}
			}

			// The operation under test.
			if err := RemoveProxiedVersionAndOrphans(ctx, doer, g.target.pv); err != nil {
				return fmt.Sprintf("RemoveProxiedVersionAndOrphans(%s): %v", g.target.label, err)
			}

			// Versions: the deleted set is gone together with its files and properties - that is
			// the "manifest and metadata" half of the property - and everything else is retained.
			for _, gv := range g.versions {
				exists, err := prop9VersionExists(ctx, gv.pv.ID)
				if err != nil {
					return fmt.Sprintf("look up version %s: %v", gv.label, err)
				}
				if g.expectDeleted[gv.pv.ID] {
					if exists {
						fail("version %s should have been deleted but still exists", gv.label)
					}
					files, err := packages_model.GetFilesByVersionID(ctx, gv.pv.ID)
					if err != nil {
						return fmt.Sprintf("list files of %s: %v", gv.label, err)
					}
					if len(files) != 0 {
						fail("version %s was deleted but still has %d package_file rows", gv.label, len(files))
					}
					props, err := packages_model.GetProperties(ctx, packages_model.PropertyTypeVersion, gv.pv.ID)
					if err != nil {
						return fmt.Sprintf("list properties of %s: %v", gv.label, err)
					}
					if len(props) != 0 {
						fail("version %s was deleted but still has %d version properties", gv.label, len(props))
					}
				} else if !exists {
					fail("version %s should have been retained but is gone", gv.label)
				}
			}

			// The property itself: every blob a retained version references is still present and
			// still referenced by a live package_file row.
			for id := range retainedBlobs {
				exists, err := prop9BlobExists(ctx, id)
				if err != nil {
					return fmt.Sprintf("look up blob %d: %v", id, err)
				}
				if !exists {
					fail("blob %d is referenced by a retained version but was deleted", id)
					continue
				}
				refs, err := prop9CountFilesReferencingBlob(ctx, id)
				if err != nil {
					return fmt.Sprintf("count files of blob %d: %v", id, err)
				}
				if refs < 1 {
					fail("blob %d is referenced by a retained version but has no package_file row left", id)
				}
			}

			// Non-vacuity, part 2: the delete really did strand blobs, so the check above is not
			// satisfied by "nothing changed at all".
			if len(newlyOrphaned) == 0 {
				fail("setup is vacuous: the delete stranded no blob at all")
			}

			// The deferred handoff: stranded blobs still exist, and among this iteration's blobs
			// they are exactly what the cleanup cron would collect. A negative olderThan makes the
			// cron's age threshold pass for rows created moments ago.
			expired, err := packages_model.FindExpiredUnreferencedBlobs(ctx, -time.Minute)
			if err != nil {
				return fmt.Sprintf("FindExpiredUnreferencedBlobs: %v", err)
			}
			collectable := map[int64]bool{}
			for _, pb := range expired {
				if g.allBlobIDs[pb.ID] {
					collectable[pb.ID] = true
				}
			}
			for id := range newlyOrphaned {
				exists, err := prop9BlobExists(ctx, id)
				if err != nil {
					return fmt.Sprintf("look up stranded blob %d: %v", id, err)
				}
				if !exists {
					fail("blob %d was deleted synchronously, but blob removal is deferred to the cleanup cron", id)
				}
				if !collectable[id] {
					fail("blob %d lost its last reference but the cleanup cron would not collect it", id)
				}
			}
			for id := range collectable {
				if !newlyOrphaned[id] {
					fail("blob %d would be collected by the cleanup cron although it did not lose its last reference", id)
				}
			}
			if len(collectable) != len(newlyOrphaned) {
				fail("cron-collectable blobs %v differ from stranded blobs %v", prop9SortedIDs(collectable), prop9SortedIDs(newlyOrphaned))
			}

			return strings.Join(problems, "; ")
		},
		prop9GenGraphSpec(),
	))

	properties.TestingRun(t)
}
