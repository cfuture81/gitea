// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pypi

import (
	"encoding/base64"
	"errors"
	"html"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/log"
	packages_module "gitea.dev/modules/packages"
	pypi_module "gitea.dev/modules/packages/pypi"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
	packages_service "gitea.dev/services/packages"
)

var pypiProxyHTTPClient = &http.Client{Timeout: 180 * time.Second}

const maxPyPIFileBytes = 500 * 1024 * 1024

var pypiAnchorRe = regexp.MustCompile(`(?is)<a\s+href="([^"]+)"([^>]*)>([^<]*)</a>`)

func getEnabledPyPIUpstreams(ctx *context.Context) []*packages_model.PackageRegistryUpstream {
	if !setting.Packages.EnableUpstreamProxy {
		return nil
	}
	ups, err := packages_model.GetEnabledUpstreamsByOwnerAndType(ctx, ctx.Package.Owner.ID, packages_model.TypePyPI)
	if err != nil {
		return nil
	}
	return ups
}

func countPyPIHit(ctx *context.Context, ups []*packages_model.PackageRegistryUpstream) {
	if len(ups) == 0 {
		return
	}
	if err := packages_model.IncrUpstreamHitCount(ctx, ups[0].ID); err != nil {
		log.Error("pypi proxy: incr hit count: %v", err)
	}
}

// pypiSimpleBase normalizes the upstream URL to the PEP 503 simple-index base (…/simple).
func pypiSimpleBase(up *packages_model.PackageRegistryUpstream) string {
	base := strings.TrimSuffix(strings.TrimSpace(up.URL), "/")
	if !strings.HasSuffix(base, "/simple") {
		base += "/simple"
	}
	return base
}

func httpGetPyPI(ctx *context.Context, up *packages_model.PackageRegistryUpstream, url, accept string) (io.ReadCloser, bool) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
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
	resp, err := pypiProxyHTTPClient.Do(req)
	if err != nil {
		log.Error("pypi proxy: upstream request failed: %v", err)
		return nil, false
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, false
	}
	return resp.Body, true
}

// versionFromFilename extracts the version from a wheel/sdist filename.
func versionFromFilename(filename string) string {
	if strings.HasSuffix(filename, ".whl") || strings.HasSuffix(filename, ".egg") {
		parts := strings.Split(strings.TrimSuffix(strings.TrimSuffix(filename, ".whl"), ".egg"), "-")
		if len(parts) >= 2 {
			return parts[1]
		}
		return ""
	}
	for _, ext := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tgz", ".zip"} {
		if strings.HasSuffix(filename, ext) {
			stem := strings.TrimSuffix(filename, ext)
			if i := strings.LastIndex(stem, "-"); i >= 0 {
				return stem[i+1:]
			}
			return ""
		}
	}
	return ""
}

// serveProxiedSimpleIndex fetches the upstream PEP 503 index for pull_through and rewrites each
// file link to this registry's /pypi/files/… endpoint, carrying the real upstream file URL in a
// url-safe base64 `u` query param (the #sha256 fragment is preserved for pip's hash check).
func serveProxiedSimpleIndex(ctx *context.Context, up *packages_model.PackageRegistryUpstream, name string) bool {
	rc, ok := httpGetPyPI(ctx, up, pypiSimpleBase(up)+"/"+name+"/", "text/html")
	if !ok {
		return false
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if err != nil {
		return false
	}

	registry := strings.TrimSuffix(setting.AppURL, "/") + "/api/packages/" + ctx.Package.Owner.Name + "/pypi"
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head><title>Links for " + html.EscapeString(name) + "</title></head><body>\n")
	b.WriteString("<h1>Links for " + html.EscapeString(name) + "</h1>\n")
	for _, m := range pypiAnchorRe.FindAllStringSubmatch(string(body), -1) {
		href, attrs, text := html.UnescapeString(m[1]), m[2], strings.TrimSpace(m[3])
		upURL, frag := href, ""
		if i := strings.IndexByte(href, '#'); i >= 0 {
			upURL, frag = href[:i], href[i:]
		}
		filename := text
		if filename == "" {
			filename = path.Base(upURL)
		}
		version := versionFromFilename(filename)
		if version == "" || !isValidNameAndVersion(name, version) {
			continue
		}
		enc := base64.RawURLEncoding.EncodeToString([]byte(upURL))
		newHref := registry + "/files/" + name + "/" + version + "/" + filename + "?u=" + enc + frag
		b.WriteString(`<a href="` + html.EscapeString(newHref) + `"` + attrs + `>` + html.EscapeString(filename) + "</a><br>\n")
	}
	b.WriteString("</body></html>\n")

	ctx.Resp.Header().Set("Content-Type", "text/html; charset=utf-8")
	ctx.Resp.WriteHeader(http.StatusOK)
	_, _ = ctx.Resp.Write([]byte(b.String()))
	return true
}

// proxyFetchPyPIFile fetches a single distribution file from the upstream (URL carried in the
// rewritten link) and stores it as a normal PyPI version file. Returns true if stored.
func proxyFetchPyPIFile(ctx *context.Context, up *packages_model.PackageRegistryUpstream, name, version, filename, upstreamURL string) bool {
	if up.Mode != packages_model.UpstreamModePullThrough || upstreamURL == "" {
		return false
	}
	releaser, err := globallock.Lock(ctx, "pypi_proxy_"+strconv.FormatInt(up.OwnerID, 10)+"_"+strings.ToLower(name)+"_"+strings.ToLower(filename))
	if err != nil {
		return false
	}
	defer releaser()

	rc, ok := httpGetPyPI(ctx, up, upstreamURL, "")
	if !ok {
		return false
	}
	defer rc.Close()

	buf, err := packages_module.CreateHashedBufferFromReader(io.LimitReader(rc, maxPyPIFileBytes))
	if err != nil {
		log.Error("pypi proxy: buffer file: %v", err)
		return false
	}
	defer buf.Close()

	_, _, err = packages_service.CreatePackageOrAddFileToExisting(ctx,
		&packages_service.PackageCreationInfo{
			PackageInfo: packages_service.PackageInfo{
				Owner:       ctx.Package.Owner,
				PackageType: packages_model.TypePyPI,
				Name:        name,
				Version:     version,
			},
			SemverCompatible: false,
			Creator:          ctx.Package.Owner,
			Metadata:         &pypi_module.Metadata{},
		},
		&packages_service.PackageFileCreationInfo{
			PackageFileInfo: packages_service.PackageFileInfo{Filename: filename},
			Creator:         ctx.Package.Owner,
			Data:            buf,
			IsLead:          true,
		},
	)
	if err != nil && !errors.Is(err, packages_model.ErrDuplicatePackageFile) {
		log.Error("pypi proxy: store file %s/%s/%s: %v", name, version, filename, err)
		return false
	}

	if err := packages_model.IncrUpstreamFetchCount(ctx, up.ID); err != nil {
		log.Error("pypi proxy: incr fetch count: %v", err)
	}
	log.Info("pypi proxy: cached %s %s (%s) from upstream (owner %d)", name, version, filename, up.OwnerID)
	return true
}

// decodeUpstreamFileURL decodes the `u` query param set by serveProxiedSimpleIndex.
func decodeUpstreamFileURL(enc string) string {
	if enc == "" {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return ""
	}
	return string(b)
}
