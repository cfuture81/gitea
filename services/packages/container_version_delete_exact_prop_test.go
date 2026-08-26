// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	container_module "gitea.dev/modules/packages/container"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/modules/util"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// Bounds of a generated version graph. They are the mask widths too: a child set is carried as a
// bitmask, so ChildCount must stay below the width of an int on every supported platform.
const (
	maxDeleteGraphTags     = 4
	maxDeleteGraphChildren = 6
)

// deleteGraphUpstreamName is the upstream name written into the "upstream.source" property of
// proxied versions - the provenance marker Requirement 8.6 requires to disappear with its version.
const deleteGraphUpstreamName = "dockerhub"

// deleteGraphSpec is one generated container version graph: TagCount tagged index versions over a
// pool of ChildCount per-arch child manifests, plus the choice of which index version gets deleted.
//
// Child sets are bitmasks over the pool, which is what makes the three cases the cascade has to
// distinguish expressible - and independently generatable - in one shape:
//
//   - a child in the target's mask and in no other mask is EXCLUSIVE and must be cascaded away;
//   - a child in the target's mask and in another index version's mask is SHARED and must survive;
//   - a child in PinnedMask carries container.manifest.tagged and must survive even when exclusive.
type deleteGraphSpec struct {
	TagCount   int   // number of tagged index versions, 2..maxDeleteGraphTags
	ChildCount int   // size of the per-arch child pool, 2..maxDeleteGraphChildren
	ChildMasks []int // ChildMasks[i] = children referenced by index version i
	PinnedMask int   // children additionally carrying container.manifest.tagged
	// Which versions carry upstream.cached / upstream.source. The target index version is proxied
	// unconditionally (see the property comment); these masks vary the rest so retained versions do
	// not all have identical property sets.
	TagProxiedMask   int
	ChildProxiedMask int
	TargetIndex      int // index of the version to delete, 0..TagCount-1
}

// childBits is the all-ones mask over the generated child pool.
func (s deleteGraphSpec) childBits() int { return (1 << s.ChildCount) - 1 }

// referencedByOthers is the union of the child masks of every index version except the target: the
// children a retained version still needs.
func (s deleteGraphSpec) referencedByOthers() int {
	var mask int
	for i, m := range s.ChildMasks {
		if i != s.TargetIndex {
			mask |= m
		}
	}
	return mask
}

// isValidDeleteGraph reports whether a spec describes a graph the property can build.
//
// It exists because gopter's Gen.Map reuses the SOURCE generator's shrinker when the mapping keeps
// the type, so a shrunk candidate reaches the property WITHOUT passing through
// normalizeDeleteGraph. Attaching this as a SuchThat sieve makes gopter discard those candidates
// (prop.shrinkValue filters every shrink by the generator's sieve), which keeps validity a property
// of the generator rather than something the property body has to re-check.
func isValidDeleteGraph(spec deleteGraphSpec) bool {
	if spec.TagCount < 2 || spec.TagCount > maxDeleteGraphTags {
		return false
	}
	if spec.ChildCount < 2 || spec.ChildCount > maxDeleteGraphChildren {
		return false
	}
	if len(spec.ChildMasks) != spec.TagCount {
		return false
	}
	if spec.TargetIndex < 0 || spec.TargetIndex >= spec.TagCount {
		return false
	}
	bits := spec.childBits()
	for _, m := range spec.ChildMasks {
		if m <= 0 || m&^bits != 0 {
			return false // an index version with no children is not a manifest list
		}
	}
	return spec.PinnedMask&^bits == 0 && spec.ChildProxiedMask&^bits == 0 &&
		spec.TagProxiedMask&^((1<<spec.TagCount)-1) == 0
}

// normalizeDeleteGraph turns raw generated numbers into a valid graph: masks are cut to the
// generated pool size, every index version references at least one child (an index version with no
// children is not a manifest list), and the target index is inside the generated tag count.
//
// It runs as a Gen.Map so the generator only ever yields valid graphs.
func normalizeDeleteGraph(spec deleteGraphSpec) deleteGraphSpec {
	bits := spec.childBits()

	masks := make([]int, spec.TagCount)
	for i := range masks {
		m := spec.ChildMasks[i] & bits
		if m == 0 {
			m = 1 << (i % spec.ChildCount)
		}
		masks[i] = m
	}
	spec.ChildMasks = masks

	spec.PinnedMask &= bits
	spec.ChildProxiedMask &= bits
	spec.TagProxiedMask &= (1 << spec.TagCount) - 1
	spec.TargetIndex %= spec.TagCount
	return spec
}

// genDeleteGraph composes the version-graph generator from gopter's gen combinators only. The raw
// mask fields are generated at full width (maxDeleteGraphTags / maxDeleteGraphChildren) and narrowed
// by normalizeDeleteGraph, which keeps the field generators independent of each other.
func genDeleteGraph() gopter.Gen {
	maskGen := gen.IntRange(0, (1<<maxDeleteGraphChildren)-1)
	return gen.Struct(reflect.TypeOf(deleteGraphSpec{}), map[string]gopter.Gen{
		"TagCount":         gen.IntRange(2, maxDeleteGraphTags),
		"ChildCount":       gen.IntRange(2, maxDeleteGraphChildren),
		"ChildMasks":       gen.SliceOfN(maxDeleteGraphTags, maskGen),
		"PinnedMask":       gen.Frequency(map[int]gopter.Gen{4: gen.Const(0), 3: maskGen}),
		"TagProxiedMask":   gen.IntRange(0, (1<<maxDeleteGraphTags)-1),
		"ChildProxiedMask": maskGen,
		"TargetIndex":      gen.IntRange(0, maxDeleteGraphTags-1),
	}).Map(normalizeDeleteGraph).SuchThat(isValidDeleteGraph)
}

// seedContainerSpecializationForDeleteExact registers the default package specialization for the
// container type. RemovePackageVersion consults the specialization registry, and
// SpecManagerType.Get panics while that registry is empty; a live instance fills it from
// pkgspec.InitManager, which this package cannot import (pkgspec imports services/packages, so it
// would be an import cycle).
//
// Registering the DEFAULT specialization reproduces production behaviour exactly: InitManager
// registers only debian and terraform, so Get(TypeContainer) already falls back to &specDefault{}
// on a live instance. Repeated calls just overwrite the same map entry, so this is safe to call
// from every test in the package.
func seedContainerSpecializationForDeleteExact() {
	GetSpecManager().Add(packages_model.TypeContainer, &specDefault{})
}

// versionSnapshot is a version plus its property set as it stood before the deletion, so a retained
// version can be checked to be byte-identical afterwards and not merely present.
type versionSnapshot struct {
	id    int64
	label string
	props []string
}

// snapshotVersionProperties reads a version's properties as a sorted "name=value" list.
func snapshotVersionProperties(ctx context.Context, versionID int64) ([]string, error) {
	pps, err := packages_model.GetProperties(ctx, packages_model.PropertyTypeVersion, versionID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pps))
	for _, pp := range pps {
		out = append(out, pp.Name+"="+pp.Value)
	}
	slices.Sort(out)
	return out, nil
}

// insertContainerVersion adds one container version. MetadataJSON is the empty JSON object because
// GetPackageDescriptor - reached through RemovePackageVersion - unmarshals it into a
// container.Metadata; the graph under test lives in the version properties, not in the metadata.
func insertContainerVersion(ctx context.Context, p *packages_model.Package, creator *user_model.User, reference string) (*packages_model.PackageVersion, error) {
	return packages_model.GetOrInsertVersion(ctx, &packages_model.PackageVersion{
		PackageID:    p.ID,
		CreatorID:    creator.ID,
		Version:      reference,
		LowerVersion: strings.ToLower(reference),
		MetadataJSON: "{}",
	})
}

// Feature: proxy-registry-admin-and-orgs, Property 7: Delete removes exactly the target version
//
// For any package with a set of versions, deleting a selected version SHALL remove that version and
// its associated properties (including `upstream.cached` / `upstream.source`) and SHALL retain every
// other version.
//
// **Validates: Requirements 8.1, 8.6**
//
// ON THE MEANING OF "EVERY OTHER VERSION": RemoveProxiedVersionAndOrphans deliberately cascades, so
// the retained set is not "everything except the target" but "everything except the target and the
// per-arch children only the target referenced". That cascade is Requirement 8.5's manifest-level
// half, and it is exactly why this property has to compute the expected survivor set instead of
// asserting a count: a test that demanded all children survive would contradict the design, and one
// that ignored them would not notice over-deletion. The generated graph therefore carries child
// membership as bitmasks, from which the two sets follow arithmetically:
//
//	removed   = {target} + {c : c in target's mask, c in no other mask, c not pinned}
//	retained  = every other version, i.e. shared children, pinned children, children of other
//	            index versions only, and all non-target index versions
//
// and every generated version lands in exactly one of them - so "no more, no less" is asserted, not
// approximated. A remaining-version count is compared as well, which catches any version the
// implementation might delete that this test did not enumerate.
//
// The target index version always carries the proxy provenance markers so the Requirement 8.6 half
// is exercised on every one of the 100+ iterations rather than only on those where a mask happened
// to set the bit; the other versions' provenance varies, which is what makes the "retained versions
// keep their properties unchanged" check meaningful instead of comparing uniform property sets.
func TestPropertyContainerDeleteRemovesExactlyTargetVersion(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	seedContainerSpecializationForDeleteExact()
	// The delete path clears the proxy negative cache for the removed references, which is a no-op
	// while the feature flag is off. Enabling it exercises the same code a live proxied delete takes.
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	ctx := t.Context()
	doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	// Non-vacuity counters: each of the three cascade cases must really have occurred.
	var iteration, sawCascade, sawShared, sawPinnedKept atomic.Int64

	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100
	properties := gopter.NewProperties(params)
	properties.Property("deleting one version removes exactly it and its orphaned children", prop.ForAll(
		func(spec deleteGraphSpec) string {
			pkgName := fmt.Sprintf("prop-delete-exact-%d", iteration.Add(1))
			p, err := packages_model.TryInsertPackage(ctx, &packages_model.Package{
				OwnerID:   doer.ID,
				Type:      packages_model.TypeContainer,
				Name:      pkgName,
				LowerName: pkgName,
			})
			if err != nil {
				return fmt.Sprintf("TryInsertPackage(%s): %v", pkgName, err)
			}

			// Per-arch child manifests, addressed by digest as the registry stores them. Digests are
			// derived from the unique package name so no two iterations share a version name.
			digests := make([]string, spec.ChildCount)
			for i := range digests {
				sum := sha256.Sum256([]byte(fmt.Sprintf("%s/child/%d", pkgName, i)))
				digests[i] = "sha256:" + hex.EncodeToString(sum[:])
			}

			snapshots := make([]versionSnapshot, 0, spec.ChildCount+spec.TagCount)
			childIDs := make([]int64, spec.ChildCount)

			for i, dgst := range digests {
				pv, err := insertContainerVersion(ctx, p, doer, dgst)
				if err != nil {
					return fmt.Sprintf("insert child version %s: %v", dgst, err)
				}
				childIDs[i] = pv.ID
				if spec.PinnedMask&(1<<i) != 0 {
					// A child that also carries a tag: the implementation must never remove it
					// implicitly, only when it is itself the selected version.
					if err := packages_model.InsertOrUpdateProperty(ctx, packages_model.PropertyTypeVersion, pv.ID, container_module.PropertyManifestTagged, ""); err != nil {
						return fmt.Sprintf("tag child %s: %v", dgst, err)
					}
				}
				if spec.ChildProxiedMask&(1<<i) != 0 {
					if err := packages_model.TagVersionCached(ctx, pv.ID, deleteGraphUpstreamName); err != nil {
						return fmt.Sprintf("mark child %s proxied: %v", dgst, err)
					}
				}
			}

			tagIDs := make([]int64, spec.TagCount)
			for i := range spec.TagCount {
				tag := fmt.Sprintf("tag-%d", i)
				pv, err := insertContainerVersion(ctx, p, doer, tag)
				if err != nil {
					return fmt.Sprintf("insert index version %s: %v", tag, err)
				}
				tagIDs[i] = pv.ID
				if err := packages_model.InsertOrUpdateProperty(ctx, packages_model.PropertyTypeVersion, pv.ID, container_module.PropertyManifestTagged, ""); err != nil {
					return fmt.Sprintf("tag index version %s: %v", tag, err)
				}
				for j := range spec.ChildCount {
					if spec.ChildMasks[i]&(1<<j) == 0 {
						continue
					}
					if _, err := packages_model.InsertProperty(ctx, packages_model.PropertyTypeVersion, pv.ID, container_module.PropertyManifestReference, digests[j]); err != nil {
						return fmt.Sprintf("reference %s from %s: %v", digests[j], tag, err)
					}
				}
				if i == spec.TargetIndex || spec.TagProxiedMask&(1<<i) != 0 {
					if err := packages_model.TagVersionCached(ctx, pv.ID, deleteGraphUpstreamName); err != nil {
						return fmt.Sprintf("mark index version %s proxied: %v", tag, err)
					}
				}
			}

			// Expected outcome, derived from the masks before anything is deleted.
			targetID := tagIDs[spec.TargetIndex]
			targetMask := spec.ChildMasks[spec.TargetIndex]
			otherMask := spec.referencedByOthers()

			expectRemoved := map[int64]bool{targetID: true}
			for j := range spec.ChildCount {
				bit := 1 << j
				switch {
				case targetMask&bit == 0: // not a child of the target: untouched
				case otherMask&bit != 0: // shared with a retained index version
					sawShared.Add(1)
				case spec.PinnedMask&bit != 0: // tagged child, never removed implicitly
					sawPinnedKept.Add(1)
				default: // exclusive to the target: orphaned by the deletion
					expectRemoved[childIDs[j]] = true
					sawCascade.Add(1)
				}
			}

			// Snapshot every version of the package, so a retained one can be compared field for
			// field and not merely counted.
			for i, id := range childIDs {
				props, err := snapshotVersionProperties(ctx, id)
				if err != nil {
					return fmt.Sprintf("snapshot child %s: %v", digests[i], err)
				}
				snapshots = append(snapshots, versionSnapshot{id: id, label: "child " + digests[i], props: props})
			}
			for i, id := range tagIDs {
				props, err := snapshotVersionProperties(ctx, id)
				if err != nil {
					return fmt.Sprintf("snapshot index version tag-%d: %v", i, err)
				}
				snapshots = append(snapshots, versionSnapshot{id: id, label: fmt.Sprintf("index version tag-%d", i), props: props})
			}

			target, err := packages_model.GetVersionByID(ctx, targetID)
			if err != nil {
				return fmt.Sprintf("load target version: %v", err)
			}
			if err := RemoveProxiedVersionAndOrphans(ctx, doer, target); err != nil {
				return fmt.Sprintf("RemoveProxiedVersionAndOrphans(tag-%d): %v", spec.TargetIndex, err)
			}

			var problems []string
			for _, snap := range snapshots {
				_, err := packages_model.GetVersionByID(ctx, snap.id)
				gone := errors.Is(err, util.ErrNotExist)
				if err != nil && !gone {
					return fmt.Sprintf("GetVersionByID(%s): %v", snap.label, err)
				}
				props, err := snapshotVersionProperties(ctx, snap.id)
				if err != nil {
					return fmt.Sprintf("re-read properties of %s: %v", snap.label, err)
				}

				if expectRemoved[snap.id] {
					if !gone {
						problems = append(problems, snap.label+" survived the deletion but must be removed")
					}
					if len(props) != 0 {
						problems = append(problems, fmt.Sprintf("%s still carries properties %v after deletion", snap.label, props))
					}
					continue
				}
				if gone {
					problems = append(problems, snap.label+" was removed but must be retained")
					continue
				}
				if !slices.Equal(props, snap.props) {
					problems = append(problems, fmt.Sprintf("%s properties changed: had %v, now %v", snap.label, snap.props, props))
				}
			}

			// Requirement 8.6 spelled out on the target, which always carried both markers.
			for _, name := range []string{packages_model.PropertyUpstreamCached, packages_model.PropertyUpstreamSource} {
				pps, err := packages_model.GetPropertiesByName(ctx, packages_model.PropertyTypeVersion, targetID, name)
				if err != nil {
					return fmt.Sprintf("GetPropertiesByName(%s) of the target: %v", name, err)
				}
				if len(pps) != 0 {
					problems = append(problems, fmt.Sprintf("target version kept its %q property after deletion", name))
				}
			}

			// Nothing beyond the enumerated versions was touched or created.
			remaining, err := packages_model.CountVersions(ctx, &packages_model.PackageSearchOptions{PackageID: p.ID})
			if err != nil {
				return fmt.Sprintf("CountVersions: %v", err)
			}
			if want := int64(len(snapshots) - len(expectRemoved)); remaining != want {
				problems = append(problems, fmt.Sprintf("package has %d versions left, want %d", remaining, want))
			}

			return strings.Join(problems, "; ")
		},
		genDeleteGraph(),
	))
	properties.TestingRun(t)

	require.GreaterOrEqual(t, iteration.Load(), int64(params.MinSuccessfulTests),
		"the property must have run at least %d iterations", params.MinSuccessfulTests)
	// Each branch of the expected-survivor computation was really taken, so no assertion above was
	// vacuously true.
	require.Positive(t, sawCascade.Load(), "no iteration produced a child exclusive to the target, so the cascade was never exercised")
	require.Positive(t, sawShared.Load(), "no iteration produced a child shared with a retained version, so shared-child retention was never exercised")
	require.Positive(t, sawPinnedKept.Load(), "no iteration produced an exclusive but tagged child, so implicit-removal protection was never exercised")
}
