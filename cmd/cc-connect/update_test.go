package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		latest, current string
		want            bool
	}{
		// Basic semver
		{"v1.2.3", "v1.2.2", true},
		{"v1.2.2", "v1.2.3", false},
		{"v1.2.3", "v1.2.3", false},
		{"v2.0.0", "v1.9.9", true},

		// Pre-release vs stable
		{"v1.2.3", "v1.2.3-beta.1", true},
		{"v1.2.3-beta.1", "v1.2.3", false},

		// Pre-release numeric ordering
		{"v1.2.3-beta.10", "v1.2.3-beta.2", true},
		{"v1.2.3-beta.2", "v1.2.3-beta.10", false},
		{"v1.2.3-beta.2", "v1.2.3-beta.2", false},

		// rc > beta lexicographically
		{"v1.2.3-rc.1", "v1.2.3-beta.9", true},

		// Dev builds always upgradeable
		{"v1.0.0", "dev", true},

		// Empty
		{"", "v1.0.0", false},
		{"v1.0.0", "", false},
	}
	for _, tt := range tests {
		got := isNewer(tt.latest, tt.current)
		if got != tt.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
		}
	}
}

func TestGetUpdateHintIfAvailable_NeverBlocks(t *testing.T) {
	origVersion := version
	defer func() { version = origVersion }()
	version = "v1.0.0"

	// Clear cache to force cache miss
	cachedLatestVersion.mu.Lock()
	cachedLatestVersion.version = ""
	cachedLatestVersion.timestamp = time.Time{}
	cachedLatestVersion.mu.Unlock()

	// getUpdateHintIfAvailable should return "" immediately on cache miss
	// (async fetch is kicked off in background but does not block)
	start := time.Now()
	hint := getUpdateHintIfAvailable()
	elapsed := time.Since(start)

	if hint != "" {
		t.Errorf("expected empty hint on cache miss, got: %q", hint)
	}
	if elapsed > 2*time.Second {
		t.Errorf("getUpdateHintIfAvailable blocked for %v, should return immediately", elapsed)
	}
}

func TestGetUpdateHintIfAvailable_UsesCache(t *testing.T) {
	origVersion := version
	defer func() { version = origVersion }()
	version = "v1.0.0"

	// Populate cache with a newer version
	cachedLatestVersion.mu.Lock()
	cachedLatestVersion.version = "v2.0.0"
	cachedLatestVersion.timestamp = time.Now()
	cachedLatestVersion.mu.Unlock()

	hint := getUpdateHintIfAvailable()
	if hint == "" {
		t.Error("expected update hint when cache has newer version")
	}

	// Populate cache with same version — should return empty
	cachedLatestVersion.mu.Lock()
	cachedLatestVersion.version = "v1.0.0"
	cachedLatestVersion.timestamp = time.Now()
	cachedLatestVersion.mu.Unlock()

	hint = getUpdateHintIfAvailable()
	if hint != "" {
		t.Errorf("expected no hint when versions match, got: %q", hint)
	}
}

func TestGetUpdateHintIfAvailable_DevSkipped(t *testing.T) {
	origVersion := version
	defer func() { version = origVersion }()
	version = "dev"

	hint := getUpdateHintIfAvailable()
	if hint != "" {
		t.Errorf("expected empty hint for dev version, got: %q", hint)
	}
}

func TestSyncNpmPackageVersion_NormalizesVPrefix(t *testing.T) {
	// Regression test: old package.json stored version as "v1.0.0" but newVer
	// is already stripped to "1.0.0". They should be treated as equal.
	dir := t.TempDir()
	ccConnectDir := filepath.Join(dir, "node_modules", "cc-connect")
	binDir := filepath.Join(ccConnectDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	execPath := filepath.Join(binDir, "cc-connect")

	pkgJSON := filepath.Join(ccConnectDir, "package.json")
	pkgData := `{"name": "cc-connect", "version": "v1.0.0"}`
	if err := os.WriteFile(pkgJSON, []byte(pkgData), 0o644); err != nil {
		t.Fatalf("write pkg.json: %v", err)
	}

	// newVer has "v" already stripped: "1.0.0" vs package.json "v1.0.0"
	syncNpmPackageVersion(execPath, "1.0.0")

	// Re-read and verify version was NOT overwritten (same version)
	content, err := os.ReadFile(pkgJSON)
	if err != nil {
		t.Fatalf("read pkg.json: %v", err)
	}
	var pkg map[string]any
	if err := json.Unmarshal(content, &pkg); err != nil {
		t.Fatalf("parse pkg.json: %v", err)
	}
	// Version should still be "v1.0.0" (not overwritten with "1.0.0")
	if pkg["version"] != "v1.0.0" {
		t.Errorf("version = %v, want v1.0.0 (unchanged)", pkg["version"])
	}
}

func TestNormalizeVersionTag(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"v1.2.3", "v1.2.3"},
		{"1.2.3", "v1.2.3"},
		{"  1.2.3  ", "v1.2.3"},
		{"v1.2.3-beta.1", "v1.2.3-beta.1"},
		{"latest", "latest"},
		{"latest-abc1234", "latest-abc1234"},
	}
	for _, tt := range tests {
		got := normalizeVersionTag(tt.in)
		if got != tt.want {
			t.Errorf("normalizeVersionTag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseUpdateArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantErr   bool
		wantOpts  updateOpts
		errSubstr string
	}{
		{
			name:     "no args defaults to stable",
			args:     nil,
			wantOpts: updateOpts{channel: "stable"},
		},
		{
			name:     "pre flag",
			args:     []string{"--pre"},
			wantOpts: updateOpts{channel: "stable", pre: true},
		},
		{
			name:     "beta is alias for pre",
			args:     []string{"--beta"},
			wantOpts: updateOpts{channel: "stable", pre: true},
		},
		{
			name:     "channel latest",
			args:     []string{"--channel", "latest"},
			wantOpts: updateOpts{channel: "latest"},
		},
		{
			name:     "version pin normalizes leading v",
			args:     []string{"--version", "1.2.3"},
			wantOpts: updateOpts{channel: "stable", pinVersion: "v1.2.3"},
		},
		{
			name:     "version pin with leading v unchanged",
			args:     []string{"--version", "v1.2.3"},
			wantOpts: updateOpts{channel: "stable", pinVersion: "v1.2.3"},
		},
		{
			name:      "version + channel latest is rejected",
			args:      []string{"--version", "v1.2.3", "--channel", "latest"},
			wantErr:   true,
			errSubstr: "mutually exclusive",
		},
		{
			name:      "version + pre is rejected",
			args:      []string{"--version", "v1.2.3", "--pre"},
			wantErr:   true,
			errSubstr: "mutually exclusive",
		},
		{
			name:      "channel latest + pre is rejected",
			args:      []string{"--channel", "latest", "--pre"},
			wantErr:   true,
			errSubstr: "mutually exclusive",
		},
		{
			name:      "unknown channel rejected",
			args:      []string{"--channel", "nightly"},
			wantErr:   true,
			errSubstr: "unknown --channel",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUpdateArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseUpdateArgs(%v) = no error, want error", tt.args)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseUpdateArgs(%v) unexpected error: %v", tt.args, err)
			}
			if got != tt.wantOpts {
				t.Errorf("parseUpdateArgs(%v) = %+v, want %+v", tt.args, got, tt.wantOpts)
			}
		})
	}
}

func TestSelectAssetURLs(t *testing.T) {
	rel := &githubRelease{
		TagName: "v1.2.3",
		Assets: []releaseAsset{
			{Name: "cc-connect-v1.2.3-linux-amd64", BrowserDownloadURL: "https://example/linux-amd64-bin"},
			{Name: "cc-connect-v1.2.3-linux-amd64.tar.gz", BrowserDownloadURL: "https://example/linux-amd64-tgz"},
			{Name: "cc-connect-v1.2.3-darwin-arm64", BrowserDownloadURL: "https://example/darwin-arm64-bin"},
			{Name: "cc-connect-v1.2.3-darwin-arm64.tar.gz", BrowserDownloadURL: "https://example/darwin-arm64-tgz"},
			{Name: "cc-connect-v1.2.3-windows-amd64.exe", BrowserDownloadURL: "https://example/win-amd64-exe"},
			{Name: "cc-connect-v1.2.3-windows-amd64.zip", BrowserDownloadURL: "https://example/win-amd64-zip"},
			{Name: "checksums.txt", BrowserDownloadURL: "https://example/checksums"},
		},
	}

	tests := []struct {
		goos, goarch         string
		wantBin, wantArchive string
	}{
		{"linux", "amd64", "https://example/linux-amd64-bin", "https://example/linux-amd64-tgz"},
		{"darwin", "arm64", "https://example/darwin-arm64-bin", "https://example/darwin-arm64-tgz"},
		{"windows", "amd64", "https://example/win-amd64-exe", "https://example/win-amd64-zip"},
		{"linux", "arm64", "", ""}, // not in this release
	}
	for _, tt := range tests {
		bin, arc := selectAssetURLs(rel, tt.goos, tt.goarch)
		if bin != tt.wantBin {
			t.Errorf("selectAssetURLs(%s/%s) bin = %q, want %q", tt.goos, tt.goarch, bin, tt.wantBin)
		}
		if arc != tt.wantArchive {
			t.Errorf("selectAssetURLs(%s/%s) archive = %q, want %q", tt.goos, tt.goarch, arc, tt.wantArchive)
		}
	}
}

func TestSelectAssetURLs_LatestChannelShaName(t *testing.T) {
	// Latest channel: tag is "latest" but asset names embed the short SHA.
	rel := &githubRelease{
		TagName: "latest",
		Name:    "latest (rolling, latest-abc1234)",
		Assets: []releaseAsset{
			{Name: "cc-connect-latest-abc1234-linux-amd64.tar.gz", BrowserDownloadURL: "https://example/latest-linux"},
			{Name: "cc-connect-latest-abc1234-darwin-arm64.tar.gz", BrowserDownloadURL: "https://example/latest-darwin"},
			{Name: "cc-connect-latest-abc1234-windows-amd64.zip", BrowserDownloadURL: "https://example/latest-win"},
		},
	}
	_, arc := selectAssetURLs(rel, "darwin", "arm64")
	if arc != "https://example/latest-darwin" {
		t.Errorf("latest-channel darwin/arm64 archive = %q, want latest-darwin", arc)
	}
}

func TestExtractLatestVersionFromAssets(t *testing.T) {
	tests := []struct {
		name   string
		assets []releaseAsset
		want   string
	}{
		{
			name: "linux asset",
			assets: []releaseAsset{
				{Name: "cc-connect-latest-abc1234-linux-amd64.tar.gz"},
			},
			want: "latest-abc1234",
		},
		{
			name: "darwin asset",
			assets: []releaseAsset{
				{Name: "cc-connect-latest-def5678-darwin-arm64.tar.gz"},
			},
			want: "latest-def5678",
		},
		{
			name: "windows asset",
			assets: []releaseAsset{
				{Name: "cc-connect-latest-abcdef0-windows-amd64.zip"},
			},
			want: "latest-abcdef0",
		},
		{
			name: "no latest assets",
			assets: []releaseAsset{
				{Name: "cc-connect-v1.2.3-linux-amd64.tar.gz"},
			},
			want: "",
		},
		{
			name:   "empty",
			assets: nil,
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rel := &githubRelease{Assets: tt.assets}
			got := extractLatestVersionFromAssets(rel)
			if got != tt.want {
				t.Errorf("extractLatestVersionFromAssets() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReleaseDisplayVersion(t *testing.T) {
	tests := []struct {
		name string
		rel  *githubRelease
		opts updateOpts
		want string
	}{
		{
			name: "stable uses tag",
			rel:  &githubRelease{TagName: "v1.2.3"},
			opts: updateOpts{channel: "stable"},
			want: "v1.2.3",
		},
		{
			name: "latest prefers asset-derived sha",
			rel: &githubRelease{
				TagName: "latest",
				Name:    "latest (rolling)",
				Assets: []releaseAsset{
					{Name: "cc-connect-latest-abc1234-linux-amd64.tar.gz"},
				},
			},
			opts: updateOpts{channel: "latest"},
			want: "latest-abc1234",
		},
		{
			name: "latest falls back to release name when assets miss",
			rel:  &githubRelease{TagName: "latest", Name: "latest-fallback"},
			opts: updateOpts{channel: "latest"},
			want: "latest-fallback",
		},
		{
			name: "latest falls back to tag when name and assets both empty",
			rel:  &githubRelease{TagName: "latest"},
			opts: updateOpts{channel: "latest"},
			want: "latest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := releaseDisplayVersion(tt.rel, tt.opts)
			if got != tt.want {
				t.Errorf("releaseDisplayVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFetchLatestStableReleaseFrom_APINotFound is a regression test for the
// "unexpected redirect" error users hit when the configured fork has no
// published stable release. A 404 from the GitHub API must produce a clear,
// actionable error and must NOT silently fall through to the HTML scrape.
func TestFetchLatestStableReleaseFrom_APINotFound(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()

	// Fail loudly if the HTML fallback is touched on a definitive 404 —
	// the whole point of the fix is to skip it.
	htmlHits := 0
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		htmlHits++
	}))
	defer html.Close()

	rel, err := fetchLatestStableReleaseFrom(api.URL, html.URL)
	if err == nil {
		t.Fatalf("expected error, got release %+v", rel)
	}
	if htmlHits != 0 {
		t.Errorf("HTML fallback called %d times on API 404, want 0", htmlHits)
	}
	msg := err.Error()
	if !strings.Contains(msg, "no stable release") {
		t.Errorf("error %q should mention 'no stable release'", msg)
	}
	if !strings.Contains(msg, "--channel latest") {
		t.Errorf("error %q should suggest '--channel latest'", msg)
	}
	if !strings.Contains(msg, "--pre") {
		t.Errorf("error %q should suggest '--pre'", msg)
	}
}

// TestFetchLatestStableReleaseFrom_HTMLRedirectsToReleasesListing covers the
// other path that produced the original "unexpected redirect" message: the
// API returns a transient 5xx, the HTML fallback runs, but GitHub redirects
// to the bare /releases listing because no stable release exists.
func TestFetchLatestStableReleaseFrom_HTMLRedirectsToReleasesListing(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer api.Close()

	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://github.com/Unimountltd/cc-connect/releases")
		w.WriteHeader(http.StatusFound)
	}))
	defer html.Close()

	_, err := fetchLatestStableReleaseFrom(api.URL, html.URL)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no stable release") {
		t.Errorf("error %q should mention 'no stable release'", msg)
	}
	if strings.Contains(msg, "unexpected redirect") {
		t.Errorf("error %q should NOT use the legacy 'unexpected redirect' wording", msg)
	}
}

// TestFetchLatestStableReleaseFrom_HTMLRedirectExtractsTag verifies the
// happy path of the HTML fallback (transient API failure, but a real stable
// release does exist). The function should parse the tag from the redirect
// Location header.
func TestFetchLatestStableReleaseFrom_HTMLRedirectExtractsTag(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer api.Close()

	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://github.com/Unimountltd/cc-connect/releases/tag/v1.2.3")
		w.WriteHeader(http.StatusFound)
	}))
	defer html.Close()

	rel, err := fetchLatestStableReleaseFrom(api.URL, html.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rel.TagName != "v1.2.3" {
		t.Errorf("TagName = %q, want v1.2.3", rel.TagName)
	}
}

// TestFetchLatestStableReleaseFrom_APISuccess verifies the happy path: API
// returns 200 with valid JSON and the HTML fallback is never touched.
func TestFetchLatestStableReleaseFrom_APISuccess(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tag_name": "v1.2.3", "name": "v1.2.3", "html_url": "https://example/v1.2.3"}`)
	}))
	defer api.Close()

	htmlHits := 0
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		htmlHits++
	}))
	defer html.Close()

	rel, err := fetchLatestStableReleaseFrom(api.URL, html.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rel.TagName != "v1.2.3" {
		t.Errorf("TagName = %q, want v1.2.3", rel.TagName)
	}
	if htmlHits != 0 {
		t.Errorf("HTML fallback called %d times on API success, want 0", htmlHits)
	}
}

func TestSyncNpmPackageVersion_UpdatesWhenDifferent(t *testing.T) {
	dir := t.TempDir()
	ccConnectDir := filepath.Join(dir, "node_modules", "cc-connect")
	binDir := filepath.Join(ccConnectDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	execPath := filepath.Join(binDir, "cc-connect")

	pkgJSON := filepath.Join(ccConnectDir, "package.json")
	pkgData := `{"name": "cc-connect", "version": "v0.9.0"}`
	if err := os.WriteFile(pkgJSON, []byte(pkgData), 0o644); err != nil {
		t.Fatalf("write pkg.json: %v", err)
	}

	syncNpmPackageVersion(execPath, "1.0.0")

	content, err := os.ReadFile(pkgJSON)
	if err != nil {
		t.Fatalf("read pkg.json: %v", err)
	}
	var pkg map[string]any
	if err := json.Unmarshal(content, &pkg); err != nil {
		t.Fatalf("parse pkg.json: %v", err)
	}
	if pkg["version"] != "1.0.0" {
		t.Errorf("version = %v, want 1.0.0 (updated)", pkg["version"])
	}
}
