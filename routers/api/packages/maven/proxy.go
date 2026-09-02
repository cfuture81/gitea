// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package maven

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/log"
	packages_module "gitea.dev/modules/packages"
	"gitea.dev/modules/packages/proxycache"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
	"gitea.dev/services/context"
	packages_service "gitea.dev/services/packages"
)

var proxyHTTPClient = &http.Client{Timeout: 60 * time.Second}

const maxMavenMetadataBytes = 16 * 1024 * 1024

// serveProxiedMavenMetadata serves version-less maven-metadata.xml from a pull_through upstream.
// Gitea otherwise builds metadata only from LOCAL versions, so proxied *plugin-group* metadata
// (e.g. org/apache/maven/plugins/maven-metadata.xml, used by Maven for plugin-prefix resolution)
// has no local package and 404s. We fetch the upstream metadata and serve it: the .xml verbatim,
// or the computed hash for the .md5/.sha1/.sha256/.sha512 sidecar (so the checksum matches the
// exact bytes we serve). Returns true if it wrote a response. First-party/hosted paths (no
// upstream match -> 404) return false so the caller falls through to the local generator.
func serveProxiedMavenMetadata(ctx *context.Context, params parameters) bool {
	ups := getEnabledMavenUpstreams(ctx)
	if len(ups) == 0 {
		return false
	}
	reqPath := ctx.PathParam("*")
	ext := strings.ToLower(path.Ext(params.Filename))
	basePath := reqPath
	if isChecksumExtension(ext) {
		basePath = strings.TrimSuffix(reqPath, ext)
	}
	for _, up := range ups {
		if up.Mode != packages_model.UpstreamModePullThrough {
			continue
		}
		remoteURL, ok := buildUpstreamURL(up.URL, basePath)
		if !ok {
			continue
		}
		negKey := strconv.FormatInt(up.ID, 10) + "|meta|" + basePath
		if proxycache.Blocked(negKey) {
			continue
		}
		body, status, ok := httpGetUpstream(ctx, up, remoteURL)
		if !ok {
			if proxycache.ShouldNegativeCache(status) {
				proxycache.Block(negKey, proxycache.NegativeCacheTTL)
			}
			continue
		}
		data, err := io.ReadAll(io.LimitReader(body, maxMavenMetadataBytes))
		body.Close()
		if err != nil {
			continue
		}
		countUpstreamHit(ctx, up)
		if isChecksumExtension(ext) {
			var hash string
			switch ext {
			case extensionMD5:
				sum := md5.Sum(data)
				hash = hex.EncodeToString(sum[:])
			case extensionSHA1:
				sum := sha1.Sum(data)
				hash = hex.EncodeToString(sum[:])
			case extensionSHA256:
				sum := sha256.Sum256(data)
				hash = hex.EncodeToString(sum[:])
			case extensionSHA512:
				sum := sha512.Sum512(data)
				hash = hex.EncodeToString(sum[:])
			}
			ctx.PlainText(http.StatusOK, hash)
			return true
		}
		ctx.Resp.Header().Set("Content-Type", contentTypeXML)
		ctx.Resp.Header().Set("Content-Length", strconv.Itoa(len(data)))
		ctx.Resp.WriteHeader(http.StatusOK)
		_, _ = ctx.Resp.Write(data)
		return true
	}
	return false
}

// getEnabledMavenUpstreams returns the owner's enabled maven upstreams in resolution order
// (priority asc), or nil if the feature is off / none configured / lookup fails.
func getEnabledMavenUpstreams(ctx *context.Context) []*packages_model.PackageRegistryUpstream {
	if !setting.Packages.EnableUpstreamProxy {
		return nil
	}
	ups, err := packages_model.GetEnabledUpstreamsByOwnerAndType(ctx, ctx.Package.Owner.ID, packages_model.TypeMaven)
	if err != nil {
		return nil
	}
	return ups
}

// countUpstreamHit increments the served-from-cache counter (observability), when an upstream
// is configured for this owner/type.
func countUpstreamHit(ctx *context.Context, up *packages_model.PackageRegistryUpstream) {
	if up == nil {
		return
	}
	if err := packages_model.IncrUpstreamHitCount(ctx, up.ID); err != nil {
		log.Error("maven proxy: incr hit count: %v", err)
	}
}

// proxyFetchAndStore fetches the requested path from the upstream and stores it in the local
// package registry. It only fetches in pull_through mode (frozen never contacts the upstream).
// Returns true if a file was stored, so the caller can retry the local serve.
func proxyFetchAndStore(ctx *context.Context, params parameters, up *packages_model.PackageRegistryUpstream) bool {
	if up == nil || up.Mode != packages_model.UpstreamModePullThrough {
		return false
	}

	reqPath := ctx.PathParam("*")
	filename := params.Filename
	ext := strings.ToLower(path.Ext(filename))
	if isChecksumExtension(ext) {
		// Fetch/cache the base file; checksums are served locally from the stored blob hashes.
		filename = filename[:len(filename)-len(ext)]
		reqPath = strings.TrimSuffix(reqPath, ext)
		params.Filename = filename
	}

	// Serialize concurrent proxy fetches for the same path (singleflight-style).
	releaser, err := globallock.Lock(ctx, "maven_proxy_"+strconv.FormatInt(up.OwnerID, 10)+"_"+reqPath)
	if err != nil {
		return false
	}
	defer releaser()

	// Another request may have cached it while we waited for the lock.
	if localFileExists(ctx, params) {
		return true
	}

	remoteURL, ok := buildUpstreamURL(up.URL, reqPath)
	if !ok {
		log.Warn("maven proxy: refusing non-http(s) upstream URL for %s", up.URL)
		return false
	}

	negKey := strconv.FormatInt(up.ID, 10) + "|" + reqPath
	if proxycache.Blocked(negKey) {
		log.Info("maven proxy: negative-cache short-circuit for %s (upstream %q)", reqPath, up.Name)
		return false
	}
	body, status, ok := httpGetUpstream(ctx, up, remoteURL)
	if !ok {
		if proxycache.ShouldNegativeCache(status) {
			proxycache.Block(negKey, proxycache.NegativeCacheTTL)
		}
		return false
	}
	defer body.Close()

	buf, err := packages_module.CreateHashedBufferFromReader(body)
	if err != nil {
		log.Error("maven proxy: buffer error: %v", err)
		return false
	}
	defer buf.Close()

	pvci := &packages_service.PackageCreationInfo{
		PackageInfo: packages_service.PackageInfo{
			Owner:       ctx.Package.Owner,
			PackageType: packages_model.TypeMaven,
			Name:        params.toInternalPackageName(),
			Version:     params.Version,
		},
		Creator: ctx.Package.Owner,
	}
	pfci := &packages_service.PackageFileCreationInfo{
		PackageFileInfo:   packages_service.PackageFileInfo{Filename: filename},
		Creator:           ctx.Package.Owner,
		Data:              buf,
		OverwriteExisting: params.IsMeta,
	}
	pv, _, err := packages_service.CreatePackageOrAddFileToExisting(ctx, pvci, pfci)
	if err != nil && !errors.Is(err, packages_model.ErrDuplicatePackageFile) {
		log.Error("maven proxy: store error: %v", err)
		return false
	}
	if pv != nil {
		if err := packages_model.TagVersionCached(ctx, pv.ID, up.Name); err != nil {
			log.Error("maven proxy: tag cached: %v", err)
		}
	}

	if err := packages_model.IncrUpstreamFetchCount(ctx, up.ID); err != nil {
		log.Error("maven proxy: incr fetch count: %v", err)
	}
	log.Info("maven proxy: cached %q from upstream %q (owner %d)", reqPath, up.URL, up.OwnerID)
	return true
}

// localFileExists reports whether the (base) file for params is already cached locally.
func localFileExists(ctx *context.Context, params parameters) bool {
	pv, err := packages_model.GetVersionByNameAndVersion(ctx, ctx.Package.Owner.ID, packages_model.TypeMaven, params.toInternalPackageName(), params.Version)
	if errors.Is(err, util.ErrNotExist) {
		pv, err = packages_model.GetVersionByNameAndVersion(ctx, ctx.Package.Owner.ID, packages_model.TypeMaven, params.toInternalPackageNameLegacy(), params.Version)
	}
	if err != nil {
		return false
	}
	_, err = packages_model.GetFileForVersionByName(ctx, pv.ID, params.Filename, packages_model.EmptyFileKey)
	return err == nil
}

func buildUpstreamURL(base, reqPath string) (string, bool) {
	base = strings.TrimSuffix(base, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return "", false
	}
	return base + "/" + strings.TrimPrefix(reqPath, "/"), true
}

// maxUpstreamAttempts bounds retries for transient upstream failures (initial + retries).
const maxUpstreamAttempts = 3

// isTransientUpstreamStatus reports whether an upstream HTTP status is worth retrying
// (rate-limit / transient server errors). 404/410/403/etc. are definitive and NOT retried.
func isTransientUpstreamStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// parseRetryAfterSeconds parses a numeric (delta-seconds) Retry-After header. HTTP-date form and
// invalid values are ignored (returns 0).
func parseRetryAfterSeconds(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return 0
}

// upstreamBackoff computes the wait before the next attempt: exponential base (200ms,400ms,800ms)
// raised to at least Retry-After when the upstream provided one, capped at 2s so a client request
// never stalls for long.
func upstreamBackoff(attempt, retryAfterSecs int) time.Duration {
	d := time.Duration(200*(1<<attempt)) * time.Millisecond
	if retryAfterSecs > 0 {
		if ra := time.Duration(retryAfterSecs) * time.Second; ra > d {
			d = ra
		}
	}
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// httpGetUpstream performs the outbound GET with bounded retry+backoff on transient failures
// (transport error / 429 / 5xx). On success returns the body (caller closes) with status 200.
// On a definitive non-200 (e.g. 404/410) or after exhausting attempts it returns nil, the last
// status (0 for a transport error), false. Retries are what make big parallel builds resilient to
// momentary Central rate-limits/hiccups.
func httpGetUpstream(ctx *context.Context, up *packages_model.PackageRegistryUpstream, url string) (io.ReadCloser, int, bool) {
	lastStatus := 0
	var wait time.Duration
	for attempt := 0; attempt < maxUpstreamAttempts; attempt++ {
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, lastStatus, false
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, 0, false
		}
		req.Header.Set("User-Agent", proxycache.UserAgent)
		switch up.AuthType {
		case packages_model.UpstreamAuthBasic:
			req.SetBasicAuth(up.AuthUsername, up.AuthSecret)
		case packages_model.UpstreamAuthToken:
			req.Header.Set("Authorization", "Bearer "+up.AuthSecret)
		}
		resp, err := proxyHTTPClient.Do(req)
		if err != nil {
			log.Error("maven proxy: upstream request failed (attempt %d/%d): %v", attempt+1, maxUpstreamAttempts, err)
			lastStatus = 0
			wait = upstreamBackoff(attempt, 0)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp.Body, resp.StatusCode, true
		}
		lastStatus = resp.StatusCode
		retryAfter := parseRetryAfterSeconds(resp.Header.Get("Retry-After"))
		_ = resp.Body.Close()
		if !isTransientUpstreamStatus(lastStatus) {
			return nil, lastStatus, false
		}
		wait = upstreamBackoff(attempt, retryAfter)
	}
	return nil, lastStatus, false
}
