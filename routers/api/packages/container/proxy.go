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
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	packages_module "gitea.dev/modules/packages"
	container_module "gitea.dev/modules/packages/container"
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

	c := newUpstreamClient(up)
	if !c.fetchAndStoreManifest(ctx, image, upstreamRef, reference) {
		return false
	}

	if err := packages_model.IncrUpstreamFetchCount(ctx, up.ID); err != nil {
		log.Error("container proxy: incr fetch count: %v", err)
	}
	log.Info("container proxy: cached manifest %s:%s from upstream %q (owner %d)", image, reference, up.URL, up.OwnerID)
	return manifestExistsLocally(ctx, image, reference)
}

// proxyEnsureBlob fetches a single blob from the upstream on a cache miss (fallback path; the
// manifest pull already stores referenced blobs). Returns true if available locally afterwards.
func proxyEnsureBlob(ctx *context.Context, up *packages_model.PackageRegistryUpstream, image, dgst string) bool {
	if up == nil || up.Mode != packages_model.UpstreamModePullThrough {
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

func (c *upstreamClient) get(ctx *context.Context, urlStr, accept, scope string) (*http.Response, error) {
	doOnce := func(bearer string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return nil, err
		}
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

func (c *upstreamClient) fetchAndStoreManifest(ctx *context.Context, image, upstreamRef, storeRef string) bool {
	scope := "repository:" + image + ":pull"
	urlStr := c.base + "/v2/" + image + "/manifests/" + upstreamRef
	resp, err := c.get(ctx, urlStr, manifestAcceptHeader, scope)
	if err != nil {
		log.Error("container proxy: manifest request failed: %v", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn("container proxy: upstream manifest %s:%s status %d", image, upstreamRef, resp.StatusCode)
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize))
	if err != nil {
		log.Error("container proxy: read manifest: %v", err)
		return false
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
			return false
		}
		for _, m := range index.Manifests {
			d := m.Digest.String()
			if !c.fetchAndStoreManifest(ctx, image, d, d) {
				return false
			}
		}
		return c.storeManifest(ctx, image, storeRef, mediaType, body)
	case container_module.IsMediaTypeImageManifest(mediaType):
		var m oci.Manifest
		if err := json.Unmarshal(body, &m); err != nil {
			log.Error("container proxy: parse manifest: %v", err)
			return false
		}
		if !c.ensureBlob(ctx, image, m.Config.Digest.String()) {
			return false
		}
		for _, layer := range m.Layers {
			if !c.ensureBlob(ctx, image, layer.Digest.String()) {
				return false
			}
		}
		return c.storeManifest(ctx, image, storeRef, mediaType, body)
	default:
		log.Warn("container proxy: unsupported manifest media type %q for %s:%s", mediaType, image, upstreamRef)
		return false
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
	return true
}

func (c *upstreamClient) ensureBlob(ctx *context.Context, image, dgst string) bool {
	if blobExistsLocally(ctx, image, dgst) {
		return true
	}

	scope := "repository:" + image + ":pull"
	urlStr := c.base + "/v2/" + image + "/blobs/" + dgst
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
