// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package maven

import (
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
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
	"gitea.dev/services/context"
	packages_service "gitea.dev/services/packages"
)

var proxyHTTPClient = &http.Client{Timeout: 60 * time.Second}

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

	body, ok := httpGetUpstream(ctx, up, remoteURL)
	if !ok {
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
	if _, _, err := packages_service.CreatePackageOrAddFileToExisting(ctx, pvci, pfci); err != nil {
		if !errors.Is(err, packages_model.ErrDuplicatePackageFile) {
			log.Error("maven proxy: store error: %v", err)
			return false
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

func httpGetUpstream(ctx *context.Context, up *packages_model.PackageRegistryUpstream, url string) (io.ReadCloser, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	switch up.AuthType {
	case packages_model.UpstreamAuthBasic:
		req.SetBasicAuth(up.AuthUsername, up.AuthSecret)
	case packages_model.UpstreamAuthToken:
		req.Header.Set("Authorization", "Bearer "+up.AuthSecret)
	}
	resp, err := proxyHTTPClient.Do(req)
	if err != nil {
		log.Error("maven proxy: upstream request failed: %v", err)
		return nil, false
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, false
	}
	return resp.Body, true
}
