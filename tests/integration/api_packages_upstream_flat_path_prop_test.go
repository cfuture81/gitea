// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gitea.dev/models/organization"
	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	container_module "gitea.dev/modules/packages/container"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/structs"
	"gitea.dev/modules/test"
	"gitea.dev/tests"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

// flatPathOrgNames are the curated public orgs of Requirement 4.2. They are CREATED by the test
// (fixtures ship no org with these names) so the generated org name is a real, varying input rather
// than a re-label of one fixture row: each is an ordinary public Gitea organization owned by the
// fixture user, exactly as the operational runbook creates them.
var flatPathOrgNames = []string{"noenv", "ai-platform", "ai-playground", "5gsystems"}

// genFlatPathOrgName yields one of the curated public org names.
func genFlatPathOrgName() gopter.Gen {
	consts := make([]any, 0, len(flatPathOrgNames))
	for _, n := range flatPathOrgNames {
		consts = append(consts, n)
	}
	return gen.OneConstOf(consts...)
}

// genFlatPathImageSegment yields realistic container image path segments - first-party service
// names as well as Docker Hub library/namespace names.
func genFlatPathImageSegment() gopter.Gen {
	return gen.OneConstOf(
		"nats", "mongo", "redis", "alpine", "busybox",
		"wallet-sync", "core-app", "proxy-lab", "library", "team",
	)
}

// genFlatPathImage yields a slash-joined image name of 1..3 segments, e.g. "nats" or
// "team/wallet-sync". Multi-segment names matter here: they are what makes BOTH the flat and the
// old double-nested layout expressible as a container package name, so the property has to pin
// down which one the registry actually stores and resolves.
func genFlatPathImage() gopter.Gen {
	return gen.IntRange(1, 3).
		FlatMap(func(n any) gopter.Gen {
			return gen.SliceOfN(n.(int), genFlatPathImageSegment())
		}, reflect.TypeOf([]string{})).
		Map(func(segments []string) string {
			return strings.Join(segments, "/")
		})
}

// genFlatPathTag yields realistic container tags (all valid per the registry's reference pattern).
func genFlatPathTag() gopter.Gen {
	return gen.OneConstOf("latest", "main", "stable", "1.0", "v2.1.3", "20260101")
}

// flatPathImage is a minimal single-arch OCI image (one config blob, one layer blob, one manifest).
// Bodies embed a per-iteration nonce so every iteration has distinct digests - no iteration can be
// satisfied by content another iteration already stored.
type flatPathImage struct {
	config, layer, manifest   []byte
	cfgDigest, layerDigest    string
	manifestDigest, mediaType string
}

func flatPathSHA(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func buildFlatPathImage(nonce string) *flatPathImage {
	img := &flatPathImage{mediaType: oci.MediaTypeImageManifest}
	img.config = []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"nonce":"` + nonce + `"}`)
	img.layer = []byte("flat-path-layer-" + nonce)
	img.cfgDigest, img.layerDigest = flatPathSHA(img.config), flatPathSHA(img.layer)
	img.manifest = []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
		img.mediaType, img.cfgDigest, len(img.config), img.layerDigest, len(img.layer)))
	img.manifestDigest = flatPathSHA(img.manifest)
	return img
}

// flatPathUpstream is an in-process fake OCI registry: it serves only what a test iteration has
// explicitly published, and counts manifest requests so the test can prove whether the upstream was
// contacted (non-vacuity of the proxied half, locality of the hosted half).
type flatPathUpstream struct {
	mu           sync.Mutex
	bodies       map[string][]byte // request path -> body
	types        map[string]string // request path -> Content-Type
	manifestHits atomic.Int64
}

func newFlatPathUpstream() *flatPathUpstream {
	return &flatPathUpstream{bodies: map[string][]byte{}, types: map[string]string{}}
}

// publish makes img available at the upstream address of image:tag (RemotePrefix is empty, so the
// upstream address equals the image name).
func (f *flatPathUpstream) publish(image, tag string, img *flatPathImage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := "/v2/" + image
	f.bodies[base+"/manifests/"+tag] = img.manifest
	f.types[base+"/manifests/"+tag] = img.mediaType
	f.bodies[base+"/blobs/"+img.cfgDigest] = img.config
	f.bodies[base+"/blobs/"+img.layerDigest] = img.layer
}

func (f *flatPathUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	body, ok := f.bodies[r.URL.Path]
	ct := f.types[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if strings.Contains(r.URL.Path, "/manifests/") {
		f.manifestHits.Add(1)
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	_, _ = w.Write(body)
}

// Feature: proxy-registry-admin-and-orgs, Property 3: Flat container path storage and resolution
//
// For any organization `org`, image name `image`, and tag `t`, a container package stored under
// owner `org` with package name `image` (hosted or proxied) SHALL be resolvable at the flat path
// `<host>/<org>/<image>:<t>` with the package owner equal to `org` and the package name equal to
// `image` and no additional path segment, and SHALL NOT require the double-nested
// `<host>/registry/<org>/<image>` layout.
//
// **Validates: Requirements 5.1, 5.2, 5.3, 5.4, 7.1**
//
// Both halves of "hosted or proxied" are covered: a generated boolean decides whether the iteration
// uploads the image directly (Hosted_Image, R7.1) or lets a pull-through miss cache it from the
// in-process fake upstream (Proxied_Image, R5.2). Either way the assertion is the same round-trip:
// the flat pull path resolves, and the path REBUILT from the stored row -
// "/v2/" + owner + "/" + package name + "/manifests/" + version - is byte-identical to the path
// that just resolved. That equality is what "no additional path segment" means operationally: it
// fails the moment anything inserts a namespace segment on either side.
//
// ON THE "SHALL NOT require the double-nested layout" CLAUSE: this test deliberately does NOT
// assert that `/v2/registry/<org>/<image>/...` returns 404, and a future reader should not "fix"
// that. Container image names may legally contain slashes, so `registry/<org>/<image>` is itself a
// well-formed FLAT name (owner `registry`, image `<org>/<image>`) - asserting it must not resolve
// would assert something false about the data model. The old double-nested layout was a DATA
// artifact, not a code constraint. The clause is therefore discharged positively: the flat path
// resolves on its own, and no nested-named package is created alongside it (asserted below).
func TestPropertyContainerFlatPathStorageAndResolution(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Packages.EnableUpstreamProxy, true)()

	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	upstream := newFlatPathUpstream()
	ts := httptest.NewServer(upstream)
	defer ts.Close()

	// One public org per curated name, each with an enabled pull_through container upstream whose
	// Target_Owner is the org itself (the model keeps TargetOwnerID == OwnerID).
	orgs := make(map[string]*organization.Organization, len(flatPathOrgNames))
	for _, name := range flatPathOrgNames {
		org := &organization.Organization{
			Name:       name,
			IsActive:   true,
			Type:       user_model.UserTypeOrganization,
			Visibility: structs.VisibleTypePublic,
		}
		require.NoError(t, organization.CreateOrganization(t.Context(), org, owner), "create org %q", name)
		orgs[name] = org

		_, err := packages_model.InsertUpstream(t.Context(), &packages_model.PackageRegistryUpstream{
			OwnerID: org.ID, Type: packages_model.TypeContainer, Name: "dockerhub", URL: ts.URL,
			Mode: packages_model.UpstreamModePullThrough, AuthType: packages_model.UpstreamAuthNone,
			MetadataTTL: 900, Enabled: true, Priority: 100,
		})
		require.NoError(t, err, "insert upstream for org %q", name)
	}

	// Gitea container-registry bearer token for the org owner (user-scoped, so one token serves
	// every org: the fixture user owns all of them).
	var tok struct {
		Token string `json:"token"`
	}
	resp := MakeRequest(t, NewRequest(t, "GET", setting.AppURL+"v2/token").AddBasicAuth(owner.Name), http.StatusOK)
	DecodeJSON(t, resp, &tok)
	userToken := "Bearer " + tok.Token

	// Per-iteration uniqueness: appended to the LAST image segment (keeping the generated segment
	// count intact) so no iteration can be served by an earlier iteration's warm cache, which would
	// make the proxied half vacuous.
	var iteration atomic.Int64

	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100

	properties := gopter.NewProperties(params)

	properties.Property("a container package stored under an org resolves at <host>/<org>/<image>:<tag>", prop.ForAll(
		func(orgName, imageBase, tag string, hosted bool) string {
			org := orgs[orgName]
			nonce := "i" + strconv.FormatInt(iteration.Add(1), 10)
			image := imageBase + "-" + nonce
			img := buildFlatPathImage(nonce)

			// The flat pull path: owner and image only, no intermediate segment.
			flatPath := "/v2/" + org.Name + "/" + image + "/manifests/" + tag
			flatURL := strings.TrimSuffix(setting.AppURL, "/") + flatPath

			hitsBefore := upstream.manifestHits.Load()

			if hosted {
				// Hosted_Image: push the image straight into the org at the flat path (R7.1).
				for _, blob := range []struct {
					digest string
					body   []byte
				}{{img.cfgDigest, img.config}, {img.layerDigest, img.layer}} {
					url := fmt.Sprintf("%s/v2/%s/%s/blobs/uploads?digest=%s",
						strings.TrimSuffix(setting.AppURL, "/"), org.Name, image, blob.digest)
					r := MakeRequest(t, NewRequestWithBody(t, "POST", url, strings.NewReader(string(blob.body))).
						AddTokenAuth(userToken), NoExpectedStatus)
					if r.Code != http.StatusCreated {
						return fmt.Sprintf("uploading blob %s of hosted %s/%s: status %d, want 201", blob.digest, org.Name, image, r.Code)
					}
				}
				r := MakeRequest(t, NewRequestWithBody(t, "PUT", flatURL, strings.NewReader(string(img.manifest))).
					AddTokenAuth(userToken).SetHeader("Content-Type", img.mediaType), NoExpectedStatus)
				if r.Code != http.StatusCreated {
					return fmt.Sprintf("PUT %s: status %d, want 201", flatPath, r.Code)
				}
			} else {
				// Proxied_Image: only the upstream has it; the flat pull must cache it (R5.2).
				upstream.publish(image, tag, img)
			}

			// The property: the flat path resolves.
			r := MakeRequest(t, NewRequest(t, "GET", flatURL).
				AddTokenAuth(userToken).SetHeader("Accept", img.mediaType), NoExpectedStatus)
			if r.Code != http.StatusOK {
				return fmt.Sprintf("GET %s: status %d, want 200", flatPath, r.Code)
			}
			if got := r.Body.String(); got != string(img.manifest) {
				return fmt.Sprintf("GET %s served a different manifest than stored", flatPath)
			}

			// Non-vacuity: the proxied half must really have gone to the upstream, the hosted half
			// must really have been served locally.
			hits := upstream.manifestHits.Load() - hitsBefore
			if hosted && hits != 0 {
				return fmt.Sprintf("hosted %s/%s contacted the upstream %d time(s); it must resolve locally", org.Name, image, hits)
			}
			if !hosted && hits == 0 {
				return fmt.Sprintf("proxied %s/%s never contacted the fake upstream, so nothing was fetched", org.Name, image)
			}

			// Owner is the org and the package name is the image - the stored shape behind the path.
			pv, err := packages_model.GetVersionByNameAndVersion(t.Context(), org.ID,
				packages_model.TypeContainer, strings.ToLower(image), strings.ToLower(tag))
			if err != nil {
				return fmt.Sprintf("no container version owned by org %q with name %q and version %q: %v", org.Name, image, tag, err)
			}
			pd, err := packages_model.GetPackageDescriptor(t.Context(), pv)
			if err != nil {
				return fmt.Sprintf("describing %s/%s:%s: %v", org.Name, image, tag, err)
			}
			if pd.Owner.ID != org.ID {
				return fmt.Sprintf("package owner id %d, want org %q (%d)", pd.Owner.ID, org.Name, org.ID)
			}
			if pd.Package.Name != strings.ToLower(image) {
				return fmt.Sprintf("package name %q, want %q - the image name must carry no owner/namespace prefix", pd.Package.Name, image)
			}

			// Round-trip: the path rebuilt from the stored row is the path that just resolved.
			// Exactly owner + image + reference, nothing between them.
			rebuilt := "/v2/" + pd.Owner.LowerName + "/" + pd.Package.Name + "/manifests/" + pd.Version.LowerVersion
			if rebuilt != flatPath {
				return fmt.Sprintf("path rebuilt from storage is %q but %q resolved - an extra path segment appeared", rebuilt, flatPath)
			}

			// The container repository property is the flat "<org>/<image>", never "registry/<org>/<image>".
			wantRepo := strings.ToLower(org.Name) + "/" + strings.ToLower(image)
			repos := make([]string, 0, 1)
			for _, pp := range pd.PackageProperties {
				if pp.Name == container_module.PropertyRepository {
					repos = append(repos, pp.Value)
				}
			}
			if len(repos) != 1 || repos[0] != wantRepo {
				return fmt.Sprintf("repository property %v, want exactly [%q]", repos, wantRepo)
			}

			// Positive discharge of R5.4: storing flat creates no nested-named twin alongside it.
			if _, err := packages_model.GetPackageByName(t.Context(), org.ID, packages_model.TypeContainer,
				strings.ToLower(org.Name)+"/"+strings.ToLower(image)); err == nil {
				return fmt.Sprintf("a double-nested package %q/%q was created next to the flat one", org.Name, org.Name+"/"+image)
			}
			return ""
		},
		genFlatPathOrgName(),
		genFlatPathImage(),
		genFlatPathTag(),
		gen.Bool(),
	))

	properties.TestingRun(t)

	// The property ran for real: at least MinSuccessfulTests distinct org/image/tag combinations
	// were stored and pulled, and the proxied half genuinely reached the fake upstream.
	require.GreaterOrEqual(t, iteration.Load(), int64(params.MinSuccessfulTests),
		"the property must have run at least %d iterations", params.MinSuccessfulTests)
	require.Positive(t, upstream.manifestHits.Load(),
		"the fake upstream must have been contacted by the proxied iterations")
}
