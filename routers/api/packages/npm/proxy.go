// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	packages_module "gitea.dev/modules/packages"
	npm_module "gitea.dev/modules/packages/npm"
	"gitea.dev/modules/packages/proxycache"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
	packages_service "gitea.dev/services/packages"
)

var npmProxyHTTPClient = &http.Client{Timeout: 180 * time.Second}

const maxNpmTarballBytes = 300 * 1024 * 1024

// getEnabledNpmUpstreams returns the owner's enabled npm upstreams in resolution order.
func getEnabledNpmUpstreams(ctx *context.Context) []*packages_model.PackageRegistryUpstream {
	if !setting.Packages.EnableUpstreamProxy {
		return nil
	}
	ups, err := packages_model.GetEnabledUpstreamsByOwnerAndType(ctx, ctx.Package.Owner.ID, packages_model.TypeNpm)
	if err != nil {
		return nil
	}
	return ups
}

func countNpmHit(ctx *context.Context, ups []*packages_model.PackageRegistryUpstream) {
	if len(ups) == 0 {
		return
	}
	if err := packages_model.IncrUpstreamHitCount(ctx, ups[0].ID); err != nil {
		log.Error("npm proxy: incr hit count: %v", err)
	}
}

func npmUpstreamBase(up *packages_model.PackageRegistryUpstream) string {
	return strings.TrimSuffix(strings.TrimSpace(up.URL), "/")
}

func httpGetNpm(ctx *context.Context, up *packages_model.PackageRegistryUpstream, url, accept string) (io.ReadCloser, int, bool) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, 0, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, false
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	switch up.AuthType {
	case packages_model.UpstreamAuthBasic:
		req.SetBasicAuth(up.AuthUsername, up.AuthSecret)
	case packages_model.UpstreamAuthToken:
		req.Header.Set("Authorization", "Bearer "+up.AuthSecret)
	}
	resp, err := npmProxyHTTPClient.Do(req)
	if err != nil {
		log.Error("npm proxy: upstream request failed: %v", err)
		return nil, 0, false
	}
	if resp.StatusCode != http.StatusOK {
		code := resp.StatusCode
		_ = resp.Body.Close()
		return nil, code, false
	}
	return resp.Body, resp.StatusCode, true
}

// serveProxiedPackument fetches the upstream packument for pull_through, rewrites each version's
// dist.tarball to this registry (so downloads flow through us and are cache/freeze-controlled),
// and serves it. Returns true when it wrote a response.
func serveProxiedPackument(ctx *context.Context, up *packages_model.PackageRegistryUpstream, name string) bool {
	rc, _, ok := httpGetNpm(ctx, up, npmUpstreamBase(up)+"/"+name, "application/json")
	if !ok {
		return false
	}
	defer rc.Close()

	var doc map[string]any
	if err := json.NewDecoder(rc).Decode(&doc); err != nil {
		log.Error("npm proxy: decode packument: %v", err)
		return false
	}

	registry := strings.TrimSuffix(setting.AppURL, "/") + "/api/packages/" + ctx.Package.Owner.Name + "/npm"
	if versions, ok := doc["versions"].(map[string]any); ok {
		for v, ver := range versions {
			vo, ok := ver.(map[string]any)
			if !ok {
				continue
			}
			dist, ok := vo["dist"].(map[string]any)
			if !ok {
				continue
			}
			if tb, ok := dist["tarball"].(string); ok && tb != "" {
				dist["tarball"] = registry + "/" + name + "/-/" + v + "/" + strings.ToLower(path.Base(tb))
			}
		}
	}
	ctx.JSON(http.StatusOK, doc)
	return true
}

// proxyFetchNpmTarball fetches a single tarball (by name + filename) from the upstream and stores
// it as a normal npm version (metadata parsed from the tarball's package.json). Returns true if
// the tarball is available locally afterwards.
func proxyFetchNpmTarball(ctx *context.Context, up *packages_model.PackageRegistryUpstream, name, filename string) bool {
	if up.Mode != packages_model.UpstreamModePullThrough {
		return false
	}
	releaser, err := globallock.Lock(ctx, "npm_proxy_"+strconv.FormatInt(up.OwnerID, 10)+"_"+strings.ToLower(name)+"_"+strings.ToLower(filename))
	if err != nil {
		return false
	}
	defer releaser()

	negKey := strconv.FormatInt(up.ID, 10) + "|" + name + "/-/" + filename
	if proxycache.Blocked(negKey) {
		return false
	}
	rc, status, ok := httpGetNpm(ctx, up, npmUpstreamBase(up)+"/"+name+"/-/"+filename, "")
	if !ok {
		if proxycache.ShouldNegativeCache(status) {
			proxycache.Block(negKey, proxycache.NegativeCacheTTL)
		}
		return false
	}
	defer rc.Close()
	tgz, err := io.ReadAll(io.LimitReader(rc, maxNpmTarballBytes))
	if err != nil {
		log.Error("npm proxy: read tarball: %v", err)
		return false
	}
	return storeNpmTarball(ctx, up, name, tgz)
}

// storeNpmTarball re-wraps a fetched .tgz into a publish envelope and reuses Gitea's own parser
// (ParsePackage) + persist path so version metadata is captured correctly.
func storeNpmTarball(ctx *context.Context, up *packages_model.PackageRegistryUpstream, name string, tgz []byte) bool {
	pkgJSON, err := extractPackageJSON(tgz)
	if err != nil {
		log.Error("npm proxy: extract package.json: %v", err)
		return false
	}
	var verObj map[string]any
	if err := json.Unmarshal(pkgJSON, &verObj); err != nil {
		log.Error("npm proxy: parse package.json: %v", err)
		return false
	}
	version, _ := verObj["version"].(string)
	if version == "" {
		return false
	}

	// ParsePackage validates the tarball against dist.integrity, but a raw package.json has no
	// dist. Inject the correct sha512 integrity computed from the fetched tarball.
	sum := sha512.Sum512(tgz)
	dist, _ := verObj["dist"].(map[string]any)
	if dist == nil {
		dist = map[string]any{}
	}
	dist["integrity"] = "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	verObj["dist"] = dist

	envelope := map[string]any{
		"name":      name,
		"dist-tags": map[string]string{"latest": version},
		"versions":  map[string]any{version: verObj},
		"_attachments": map[string]any{
			"package.tgz": map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tgz),
				"length":       len(tgz),
			},
		},
	}
	envBytes, err := json.Marshal(envelope)
	if err != nil {
		return false
	}
	pkg, err := npm_module.ParsePackage(bytes.NewReader(envBytes))
	if err != nil {
		log.Error("npm proxy: ParsePackage: %v", err)
		return false
	}

	buf, err := packages_module.CreateHashedBufferFromReader(bytes.NewReader(pkg.Data))
	if err != nil {
		return false
	}
	defer buf.Close()

	pv, _, err := packages_service.CreatePackageAndAddFile(ctx,
		&packages_service.PackageCreationInfo{
			PackageInfo: packages_service.PackageInfo{
				Owner:       ctx.Package.Owner,
				PackageType: packages_model.TypeNpm,
				Name:        pkg.Name,
				Version:     pkg.Version,
			},
			SemverCompatible: true,
			Creator:          ctx.Package.Owner,
			Metadata:         pkg.Metadata,
		},
		&packages_service.PackageFileCreationInfo{
			PackageFileInfo: packages_service.PackageFileInfo{Filename: pkg.Filename},
			Creator:         ctx.Package.Owner,
			Data:            buf,
			IsLead:          true,
		},
	)
	if err != nil && !errors.Is(err, packages_model.ErrDuplicatePackageVersion) && !errors.Is(err, packages_model.ErrDuplicatePackageFile) {
		log.Error("npm proxy: store tarball: %v", err)
		return false
	}
	if pv != nil {
		if err := packages_model.TagVersionCached(ctx, pv.ID, up.Name); err != nil {
			log.Error("npm proxy: tag cached: %v", err)
		}
	}

	if err := packages_model.IncrUpstreamFetchCount(ctx, up.ID); err != nil {
		log.Error("npm proxy: incr fetch count: %v", err)
	}
	log.Info("npm proxy: cached %s@%s from upstream %q (owner %d)", name, version, up.URL, up.OwnerID)
	return true
}

func extractPackageJSON(tgz []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		n := strings.TrimPrefix(h.Name, "./")
		if n == "package/package.json" {
			return io.ReadAll(io.LimitReader(tr, 4*1024*1024))
		}
	}
	return nil, fmt.Errorf("package.json not found in tarball")
}
