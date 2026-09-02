// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	packages_model "gitea.dev/models/packages"
	container_model "gitea.dev/models/packages/container"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	packages_module "gitea.dev/modules/packages"
	container_module "gitea.dev/modules/packages/container"
	"gitea.dev/modules/packages/proxycache"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
	packages_service "gitea.dev/services/packages"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
)

var containerProxyHTTPClient = &http.Client{Timeout: 300 * time.Second}

// manifestAcceptHeader advertises the manifest/index media types we understand so the upstream
// serves a form we can parse and re-store (Docker + OCI).
var manifestAcceptHeader = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// getEnabledContainerUpstreams returns the owner's enabled container upstreams in resolution
// order (priority asc), or nil if the feature is off / none configured / lookup fails.
func getEnabledContainerUpstreams(ctx *context.Context) []*packages_model.PackageRegistryUpstream {
	if !setting.Packages.EnableUpstreamProxy {
		return nil
	}
	ups, err := packages_model.GetEnabledUpstreamsByOwnerAndType(ctx, ctx.Package.Owner.ID, packages_model.TypeContainer)
	if err != nil {
		return nil
	}
	return ups
}

// upstreamTargetOwnerID returns the owner a fetch through up must be cached under. Rows written
// before the target_owner_id backfill can still carry 0, which means "unset" - the scoping OwnerID
// is the target owner in that case.
func upstreamTargetOwnerID(up *packages_model.PackageRegistryUpstream) int64 {
	if up.TargetOwnerID != 0 {
		return up.TargetOwnerID
	}
	return up.OwnerID
}

// checkUpstreamTargetOwner reports whether up's Target_Owner - the owner the fetched package would
// be created under - still exists. A false result aborts the proxy fetch, so the request falls
// through to the standard local-miss 404 instead of caching into a dangling owner (R2.5).
//
// This is an invariant guard, not a hot-path check. The model keeps TargetOwnerID == OwnerID
// (normalizeTargetOwner) and the proxy resolves upstreams by ctx.Package.Owner.ID, so on this path
// the target owner IS the owner the request already resolved to and therefore demonstrably exists -
// that case is a plain id comparison and issues no query. The lookup only runs when the two
// diverge, which the model forbids and only a direct database edit (or a future resolver that no
// longer scopes by the target owner) can produce. Keeping it here means such a divergence aborts
// the fetch with a named error instead of silently caching content under the wrong owner.
func checkUpstreamTargetOwner(ctx *context.Context, up *packages_model.PackageRegistryUpstream, image, reference string) bool {
	targetOwnerID := upstreamTargetOwnerID(up)
	if targetOwnerID == ctx.Package.Owner.ID {
		return true
	}
	if _, err := user_model.GetUserByID(ctx, targetOwnerID); err != nil {
		log.Error("container proxy: aborting fetch of %s:%s - upstream %q (id %d) targets owner id %d, which does not exist: %v",
			image, reference, up.Name, up.ID, targetOwnerID, err)
		return false
	}
	log.Error("container proxy: aborting fetch of %s:%s - upstream %q (id %d) targets owner id %d but the request resolved to owner id %d; caching here would place the package under the wrong owner",
		image, reference, up.Name, up.ID, targetOwnerID, ctx.Package.Owner.ID)
	return false
}

// countContainerUpstreamHit increments the served-from-cache counter (observability).
func countContainerUpstreamHit(ctx *context.Context, up *packages_model.PackageRegistryUpstream) {
	if up == nil {
		return
	}
	if err := packages_model.IncrUpstreamHitCount(ctx, up.ID); err != nil {
		log.Error("container proxy: incr hit count: %v", err)
	}
}

func manifestExistsLocally(ctx *context.Context, image, reference string) bool {
	opts := &container_model.BlobSearchOptions{
		OwnerID:    ctx.Package.Owner.ID,
		Image:      image,
		IsManifest: true,
	}
	if d := digest.Digest(reference); d.Validate() == nil {
		opts.Digest = string(d)
	} else {
		opts.Tag = reference
		opts.OnlyLead = true
	}
	_, err := container_model.GetContainerBlob(ctx, opts)
	return err == nil
}

func blobExistsLocally(ctx *context.Context, image, dgst string) bool {
	_, err := container_model.GetContainerBlob(ctx, &container_model.BlobSearchOptions{
		OwnerID: ctx.Package.Owner.ID,
		Image:   image,
		Digest:  dgst,
	})
	return err == nil
}

// proxyEnsureManifest fetches a manifest (and all blobs/sub-manifests it references) from the
// configured upstream on a cache miss, storing them through Gitea's normal container path.
// Only pull_through mode contacts the upstream; frozen never does. Returns true if the manifest
// is available locally afterwards, so the caller can retry the local serve.
func proxyEnsureManifest(ctx *context.Context, up *packages_model.PackageRegistryUpstream, image, reference string) bool {
	if up == nil || up.Mode != packages_model.UpstreamModePullThrough {
		return false
	}
	if !checkUpstreamTargetOwner(ctx, up, image, reference) {
		return false
	}

	releaser, err := globallock.Lock(ctx, "container_proxy_"+strconv.FormatInt(up.OwnerID, 10)+"_"+strings.ToLower(image)+"_"+reference)
	if err != nil {
		return false
	}
	defer releaser()

	// Another request may have cached it while we waited for the lock.
	if manifestExistsLocally(ctx, image, reference) {
		return true
	}

	upstreamRef := reference
	if t := up.ResolvePin(image, reference); t != "" {
		upstreamRef = t
		log.Info("container proxy: pin %s:%s -> upstream %q", image, reference, t)
	}

	negKey := strconv.FormatInt(up.ID, 10) + "|" + strings.ToLower(image) + "|" + reference
	if proxycache.Blocked(negKey) {
		return false
	}
	c := newUpstreamClient(up)
	ok, status := c.fetchAndStoreManifest(ctx, image, upstreamRef, reference)
	if !ok {
		if proxycache.ShouldNegativeCache(status) {
			proxycache.Block(negKey, proxycache.NegativeCacheTTL)
		}
		return false
	}

	if err := packages_model.IncrUpstreamFetchCount(ctx, up.ID); err != nil {
		log.Error("container proxy: incr fetch count: %v", err)
	}
	// The cached-tagging (upstream.cached / upstream.source) happens per stored version inside
	// storeManifest, which covers this tagged reference AND every per-arch sub-manifest of a
	// multi-arch index - so it is deliberately not repeated here.
	log.Info("container proxy: cached manifest %s:%s from upstream %q (owner %d)", image, reference, up.URL, up.OwnerID)
	return manifestExistsLocally(ctx, image, reference)
}

// proxyEnsureBlob fetches a single blob from the upstream on a cache miss (fallback path; the
// manifest pull already stores referenced blobs). Returns true if available locally afterwards.
func proxyEnsureBlob(ctx *context.Context, up *packages_model.PackageRegistryUpstream, image, dgst string) bool {
	if up == nil || up.Mode != packages_model.UpstreamModePullThrough {
		return false
	}
	if !checkUpstreamTargetOwner(ctx, up, image, dgst) {
		return false
	}

	releaser, err := globallock.Lock(ctx, "container_proxy_blob_"+strconv.FormatInt(up.OwnerID, 10)+"_"+strings.ToLower(image)+"_"+dgst)
	if err != nil {
		return false
	}
	defer releaser()

	if blobExistsLocally(ctx, image, dgst) {
		return true
	}

	c := newUpstreamClient(up)
	if !c.ensureBlob(ctx, image, dgst) {
		return false
	}

	if err := packages_model.IncrUpstreamFetchCount(ctx, up.ID); err != nil {
		log.Error("container proxy: incr fetch count: %v", err)
	}
	return blobExistsLocally(ctx, image, dgst)
}

// upstreamClient talks the OCI distribution API to a remote registry, handling the Bearer token
// challenge flow (e.g. Docker Hub's auth.docker.io) and caching tokens per scope.
type upstreamClient struct {
	up     *packages_model.PackageRegistryUpstream
	base   string
	tokens map[string]string
}

func newUpstreamClient(up *packages_model.PackageRegistryUpstream) *upstreamClient {
	base := strings.TrimSuffix(strings.TrimSpace(up.URL), "/")
	base = strings.TrimSuffix(base, "/v2")
	return &upstreamClient{up: up, base: base, tokens: make(map[string]string)}
}

// remoteAddr maps a local image name to the address it is requested under on the upstream,
// prepending the upstream's RemotePrefix when one is configured (e.g. image "nats" with prefix
// "noenv" is fetched as "noenv/nats"). It affects the OUTBOUND request only - the /v2/<addr>/...
// URL and the matching Bearer scope - never local storage, which stays flat under
// ctx.Package.Owner with the unprefixed image as the package name.
func (c *upstreamClient) remoteAddr(image string) string {
	return c.up.UpstreamAddr(image)
}

func (c *upstreamClient) get(ctx *context.Context, urlStr, accept, scope string) (*http.Response, error) {
	doOnce := func(bearer string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", proxycache.UserAgent)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		} else {
			switch c.up.AuthType {
			case packages_model.UpstreamAuthToken:
				req.Header.Set("Authorization", "Bearer "+c.up.AuthSecret)
			case packages_model.UpstreamAuthBasic:
				req.SetBasicAuth(c.up.AuthUsername, c.up.AuthSecret)
			}
		}
		return containerProxyHTTPClient.Do(req)
	}

	resp, err := doOnce(c.tokens[scope])
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		realm, service, chScope := parseBearerChallenge(challenge)
		if realm == "" {
			return nil, fmt.Errorf("container proxy: 401 without bearer challenge for %s", urlStr)
		}
		if chScope == "" {
			chScope = scope
		}
		tok, err := c.fetchToken(ctx, realm, service, chScope)
		if err != nil {
			return nil, err
		}
		c.tokens[scope] = tok
		return doOnce(tok)
	}
	return resp, nil
}

func (c *upstreamClient) fetchToken(ctx *context.Context, realm, service, scope string) (string, error) {
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", proxycache.UserAgent)
	if c.up.AuthType == packages_model.UpstreamAuthBasic && c.up.AuthUsername != "" {
		req.SetBasicAuth(c.up.AuthUsername, c.up.AuthSecret)
	}
	resp, err := containerProxyHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("container proxy: token endpoint status %d", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", err
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	return tok.AccessToken, nil
}

func parseBearerChallenge(h string) (realm, service, scope string) {
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return "", "", ""
	}
	for _, part := range strings.Split(h[len("Bearer "):], ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		val := strings.Trim(strings.TrimSpace(kv[1]), "\"")
		switch key {
		case "realm":
			realm = val
		case "service":
			service = val
		case "scope":
			scope = val
		}
	}
	return realm, service, scope
}

// fetchAndStoreManifest returns (stored, status) where status is the upstream manifest HTTP
// status (0 for transport/our-side errors that are transient and MUST NOT be negatively cached).
func (c *upstreamClient) fetchAndStoreManifest(ctx *context.Context, image, upstreamRef, storeRef string) (bool, int) {
	addr := c.remoteAddr(image)
	scope := "repository:" + addr + ":pull"
	urlStr := c.base + "/v2/" + addr + "/manifests/" + upstreamRef
	resp, err := c.get(ctx, urlStr, manifestAcceptHeader, scope)
	if err != nil {
		log.Error("container proxy: manifest request failed: %v", err)
		return false, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn("container proxy: upstream manifest %s:%s status %d", addr, upstreamRef, resp.StatusCode)
		return false, resp.StatusCode
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize))
	if err != nil {
		log.Error("container proxy: read manifest: %v", err)
		return false, 0
	}

	mediaType := resp.Header.Get("Content-Type")
	if !container_module.IsMediaTypeValid(mediaType) {
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(body, &probe)
		mediaType = probe.MediaType
	}

	switch {
	case container_module.IsMediaTypeImageIndex(mediaType):
		var index oci.Index
		if err := json.Unmarshal(body, &index); err != nil {
			log.Error("container proxy: parse index: %v", err)
			return false, 0
		}
		for _, m := range index.Manifests {
			d := m.Digest.String()
			if ok, _ := c.fetchAndStoreManifest(ctx, image, d, d); !ok {
				return false, 0
			}
		}
		if !c.storeManifest(ctx, image, storeRef, mediaType, body) {
			return false, 0
		}
		return true, http.StatusOK
	case container_module.IsMediaTypeImageManifest(mediaType):
		var m oci.Manifest
		if err := json.Unmarshal(body, &m); err != nil {
			log.Error("container proxy: parse manifest: %v", err)
			return false, 0
		}
		if !c.ensureBlob(ctx, image, m.Config.Digest.String()) {
			return false, 0
		}
		for _, layer := range m.Layers {
			if !c.ensureBlob(ctx, image, layer.Digest.String()) {
				return false, 0
			}
		}
		if !c.storeManifest(ctx, image, storeRef, mediaType, body) {
			return false, 0
		}
		return true, http.StatusOK
	default:
		log.Warn("container proxy: unsupported manifest media type %q for %s:%s", mediaType, image, upstreamRef)
		return false, 0
	}
}

func (c *upstreamClient) storeManifest(ctx *context.Context, image, reference, mediaType string, body []byte) bool {
	buf, err := packages_module.CreateHashedBufferFromReader(strings.NewReader(string(body)))
	if err != nil {
		log.Error("container proxy: buffer manifest: %v", err)
		return false
	}
	defer buf.Close()

	mci := &manifestCreationInfo{
		MediaType: mediaType,
		Owner:     ctx.Package.Owner,
		Creator:   ctx.Package.Owner,
		Image:     image,
		Reference: reference,
		IsTagged:  digest.Digest(reference).Validate() != nil,
	}
	if _, err := processManifest(ctx, mci, buf); err != nil {
		log.Error("container proxy: store manifest %s:%s: %v", image, reference, err)
		return false
	}
	c.tagCached(ctx, image, reference)
	return true
}

// tagCached marks the version just written by storeManifest as proxy cache content
// (upstream.cached + upstream.source = the upstream's name). Because it runs for every stored
// manifest, it covers both the tagged index reference and each per-arch sub-manifest stored under
// its digest - a multi-arch pull therefore leaves no child version that renders as first-party
// (internal) content. Best-effort: the manifest is already stored and servable at this point, so a
// property write failure is logged and never fails the pull.
func (c *upstreamClient) tagCached(ctx *context.Context, image, reference string) {
	pv, err := packages_model.GetVersionByNameAndVersion(ctx, ctx.Package.Owner.ID, packages_model.TypeContainer, strings.ToLower(image), strings.ToLower(reference))
	if err != nil {
		log.Error("container proxy: resolve cached version %s:%s: %v", image, reference, err)
		return
	}
	if err := packages_model.TagVersionCached(ctx, pv.ID, c.up.Name); err != nil {
		log.Error("container proxy: tag cached %s:%s: %v", image, reference, err)
	}
}

func (c *upstreamClient) ensureBlob(ctx *context.Context, image, dgst string) bool {
	if blobExistsLocally(ctx, image, dgst) {
		return true
	}

	addr := c.remoteAddr(image)
	scope := "repository:" + addr + ":pull"
	urlStr := c.base + "/v2/" + addr + "/blobs/" + dgst
	resp, err := c.get(ctx, urlStr, "", scope)
	if err != nil {
		log.Error("container proxy: blob request failed: %v", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn("container proxy: upstream blob %s status %d", dgst, resp.StatusCode)
		return false
	}

	buf, err := packages_module.CreateHashedBufferFromReader(resp.Body)
	if err != nil {
		log.Error("container proxy: buffer blob: %v", err)
		return false
	}
	defer buf.Close()

	pci := &packages_service.PackageCreationInfo{
		PackageInfo: packages_service.PackageInfo{
			Owner:       ctx.Package.Owner,
			PackageType: packages_model.TypeContainer,
			Name:        strings.ToLower(image),
		},
		Creator: ctx.Package.Owner,
	}
	if _, err := saveAsPackageBlob(ctx, buf, pci); err != nil {
		log.Error("container proxy: store blob %s: %v", dgst, err)
		return false
	}
	return true
}

// getManifestFromContextOrProxy returns a cached manifest, counting a cache hit when found, or
// (in pull_through mode) fetches it from the upstream on a miss and returns the freshly cached
// manifest. Frozen mode never contacts the upstream and simply misses.
//
// RESOLUTION INVARIANT (do not reorder - see below):
//
//  1. Local first. getManifestFromContext is consulted before any upstream. A locally present
//     manifest - whether first-party (hosted) or previously cached by this proxy - is served
//     without contacting any upstream at all.
//  2. On a local miss (ErrContainerBlobNotExist only), the enabled upstreams for the owner are
//     tried in the order returned by GetEnabledUpstreamsByOwnerAndType, i.e. priority ASC, id ASC.
//  3. First match wins: the loop returns as soon as one upstream yields the manifest; later
//     upstreams are not consulted.
//
// This is what makes one org behave like a Nexus docker-group: a single org endpoint blends hosted
// and pull-through-cached images, and a hosted image ALWAYS shadows a same-named upstream image.
// Reordering these steps so an upstream were consulted before the local lookup would let a public
// image silently shadow a first-party image of the same name - a supply-chain-shaped failure. Keep
// the local lookup ahead of the upstream loop across any rebase; the same invariant is recorded in
// the fork notes (FORK-MAINTENANCE.md).
func getManifestFromContextOrProxy(ctx *context.Context) (*packages_model.PackageFileDescriptor, error) {
	ups := getEnabledContainerUpstreams(ctx)
	var first *packages_model.PackageRegistryUpstream
	if len(ups) > 0 {
		first = ups[0]
	}
	manifest, err := getManifestFromContext(ctx)
	if err == nil {
		countContainerUpstreamHit(ctx, first)
		return manifest, nil
	}
	if !errors.Is(err, container_model.ErrContainerBlobNotExist) || len(ups) == 0 {
		return nil, err
	}
	image, reference := ctx.PathParam("image"), ctx.PathParam("reference")
	for _, up := range ups {
		if up.Mode != packages_model.UpstreamModePullThrough {
			continue
		}
		if proxyEnsureManifest(ctx, up, image, reference) {
			return getManifestFromContext(ctx)
		}
	}
	return nil, err
}

// getBlobFromContextOrProxy mirrors getManifestFromContextOrProxy for blobs (config/layer digests).
//
// It upholds the same RESOLUTION INVARIANT: local blob first (hosted or previously cached, served
// without contacting any upstream), then on a local miss the enabled upstreams in priority ASC,
// id ASC order, first match wins. Do not move the upstream loop ahead of getBlobFromContext on a
// rebase - see the invariant note on getManifestFromContextOrProxy and FORK-MAINTENANCE.md.
func getBlobFromContextOrProxy(ctx *context.Context) (*packages_model.PackageFileDescriptor, error) {
	ups := getEnabledContainerUpstreams(ctx)
	var first *packages_model.PackageRegistryUpstream
	if len(ups) > 0 {
		first = ups[0]
	}
	blob, err := getBlobFromContext(ctx)
	if err == nil {
		countContainerUpstreamHit(ctx, first)
		return blob, nil
	}
	if !errors.Is(err, container_model.ErrContainerBlobNotExist) || len(ups) == 0 {
		return nil, err
	}
	d := digest.Digest(ctx.PathParam("digest"))
	if d.Validate() != nil {
		return nil, err
	}
	image := ctx.PathParam("image")
	for _, up := range ups {
		if up.Mode != packages_model.UpstreamModePullThrough {
			continue
		}
		if proxyEnsureBlob(ctx, up, image, string(d)) {
			return getBlobFromContext(ctx)
		}
	}
	return nil, err
}
