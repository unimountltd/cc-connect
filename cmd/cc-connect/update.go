package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	githubRepo   = "chenhg5/cc-connect"
	githubAPI    = "https://api.github.com/repos/" + githubRepo + "/releases/latest"
	githubAllAPI = "https://api.github.com/repos/" + githubRepo + "/releases"
	githubTagAPI = "https://api.github.com/repos/" + githubRepo + "/releases/tags"
	downloadBase = "https://github.com/" + githubRepo + "/releases/download"
	giteeAPI     = "https://gitee.com/api/v5/repos/cg33/cc-connect/releases/latest"

	// mainChannelTag is the rolling pre-release tag that the main-branch CD
	// pipeline force-updates on every push to main. It is fixed; the binary
	// version inside is main-<short-sha>.
	mainChannelTag = "main"
)

// cachedLatestVersion 缓存最新版本信息，避免频繁请求API
var cachedLatestVersion struct {
	version   string
	timestamp time.Time
	mu        sync.RWMutex
}

// versionCheckTTL 缓存有效期（1小时）
const versionCheckTTL = time.Hour

type githubRelease struct {
	TagName    string         `json:"tag_name"`
	Name       string         `json:"name"`
	HTMLURL    string         `json:"html_url"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// fetchLatestStableReleaseAsync 异步获取最新稳定版本（非pre-release）
// 优先使用Gitee，如果失败则回退到GitHub
func fetchLatestStableReleaseAsync() {
	go func() {
		release, err := fetchLatestStableFromGitee()
		if err != nil || release == nil || release.TagName == "" {
			// Gitee失败，尝试GitHub
			release, err = fetchLatestStableRelease()
			if err != nil || release == nil {
				return
			}
		}
		// 缓存结果
		cachedLatestVersion.mu.Lock()
		cachedLatestVersion.version = release.TagName
		cachedLatestVersion.timestamp = time.Now()
		cachedLatestVersion.mu.Unlock()
	}()
}

// fetchLatestStableFromGitee 从Gitee获取最新稳定版本
func fetchLatestStableFromGitee() (*githubRelease, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", giteeAPI, nil)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gitee API returned HTTP %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}
	// Gitee的latest通常就是稳定版，但检查Prerelease以防万一
	if release.Prerelease {
		return nil, nil
	}
	return &release, nil
}

// checkUpdateAsync 启动异步版本检查（不阻塞）
func checkUpdateAsync() {
	// dev版本不检查
	if version == "dev" || version == "" {
		return
	}
	fetchLatestStableReleaseAsync()
}

// getUpdateHintIfAvailable returns an update hint only from cache (never blocks on network).
// Call checkUpdateAsync() early to populate the cache in the background.
func getUpdateHintIfAvailable() string {
	if version == "dev" || version == "" {
		return ""
	}

	cachedLatestVersion.mu.RLock()
	cachedVer := cachedLatestVersion.version
	cachedTime := cachedLatestVersion.timestamp
	cachedLatestVersion.mu.RUnlock()

	if cachedVer == "" || time.Since(cachedTime) > versionCheckTTL {
		// Cache miss or expired — trigger async refresh, don't block
		fetchLatestStableReleaseAsync()
		return ""
	}

	if isNewer(cachedVer, version) {
		return fmt.Sprintf("\n📦 Update available: %s → %s  (run: cc-connect update)\n", version, cachedVer)
	}
	return ""
}

func runUpdate() {
	opts, err := parseUpdateArgs(os.Args[2:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUpdateUsage(os.Stdout)
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
		printUpdateUsage(os.Stderr)
		os.Exit(2)
	}

	fmt.Printf("cc-connect %s\n", version)
	switch {
	case opts.pinVersion != "":
		fmt.Printf("Installing pinned version %s ...\n", opts.pinVersion)
	case opts.channel == "main":
		fmt.Println("Checking latest main build (rolling, unstable)...")
	case opts.pre:
		fmt.Println("Checking for updates (including pre-releases)...")
	default:
		fmt.Println("Checking for updates...")
	}

	release, err := resolveTargetRelease(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error checking updates: %v\n", err)
		os.Exit(1)
	}

	latest := release.TagName
	displayVer := releaseDisplayVersion(release, opts)

	// For pinned versions and the main channel, skip the "is newer?" guard —
	// the user explicitly asked for a specific build.
	skipNewerCheck := opts.pinVersion != "" || opts.channel == "main"

	if !skipNewerCheck {
		if !isNewer(latest, version) {
			fmt.Printf("Already up to date (%s >= %s).\n", version, latest)
			return
		}
	} else if displayVer == version {
		fmt.Printf("Already at %s.\n", displayVer)
		return
	}

	label := displayVer
	if release.Prerelease && opts.channel != "main" {
		label += " (pre-release)"
	}
	fmt.Printf("Installing: %s → %s\n", version, label)

	binURL, archiveURL := selectAssetURLs(release, runtime.GOOS, runtime.GOARCH)
	// Fall back to URL guessing for releases that don't expose an assets list
	// (e.g. the redirect-parsing fallback path of fetchLatestStableRelease).
	if binURL == "" {
		binURL = fmt.Sprintf("%s/%s/%s", downloadBase, latest, binaryAssetName(latest))
	}
	if archiveURL == "" {
		archiveURL = fmt.Sprintf("%s/%s/%s", downloadBase, latest, archiveAssetName(latest))
	}

	fmt.Printf("Downloading %s ...\n", binURL)

	tmpFile, err := downloadToTemp(binURL)
	if err != nil {
		fmt.Printf("Bare binary not found, trying archive %s ...\n", archiveURL)

		archiveTmp, archiveErr := downloadToTemp(archiveURL)
		if archiveErr != nil {
			fmt.Fprintf(os.Stderr, "Download failed: %v\n", archiveErr)
			os.Exit(1)
		}
		defer os.Remove(archiveTmp)

		tmpFile, err = extractBinaryFromArchive(archiveTmp, archiveURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Extract failed: %v\n", err)
			os.Exit(1)
		}
	}
	defer os.Remove(tmpFile)

	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot locate current binary: %v\n", err)
		os.Exit(1)
	}

	if err := replaceExecutable(execPath, tmpFile); err != nil {
		fmt.Fprintf(os.Stderr, "Update failed: %v\n", err)
		os.Exit(1)
	}

	// npm package.json sync only makes sense for stable semver tags. Main
	// builds use main-<sha> identifiers that npm wouldn't accept anyway.
	if opts.channel != "main" {
		syncNpmPackageVersion(execPath, strings.TrimPrefix(displayVer, "v"))
	}

	fmt.Printf("Updated to %s\n", displayVer)
	fmt.Println("Restart cc-connect to use the new version.")
}

// updateOpts captures the parsed CLI options for `cc-connect update`.
type updateOpts struct {
	channel    string // "stable" (default) or "main"
	pre        bool   // include pre-releases when picking latest stable
	pinVersion string // explicit tag to install (e.g. "v1.2.3"), normalized
}

// parseUpdateArgs parses the args after "update" into an updateOpts.
// It validates mutually exclusive combinations.
func parseUpdateArgs(args []string) (updateOpts, error) {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we print our own usage on error
	pre := fs.Bool("pre", false, "include pre-releases when resolving 'stable' channel")
	beta := fs.Bool("beta", false, "alias for --pre")
	channel := fs.String("channel", "stable", "release channel: stable | main")
	pinVersion := fs.String("version", "", "install a specific tag (e.g. v1.2.3)")
	if err := fs.Parse(args); err != nil {
		return updateOpts{}, err
	}

	opts := updateOpts{
		channel:    strings.ToLower(strings.TrimSpace(*channel)),
		pre:        *pre || *beta,
		pinVersion: normalizeVersionTag(*pinVersion),
	}
	if opts.channel == "" {
		opts.channel = "stable"
	}
	if opts.channel != "stable" && opts.channel != "main" {
		return updateOpts{}, fmt.Errorf("unknown --channel %q (must be 'stable' or 'main')", *channel)
	}
	if opts.pinVersion != "" && opts.channel == "main" {
		return updateOpts{}, fmt.Errorf("--version and --channel main are mutually exclusive")
	}
	if opts.pinVersion != "" && opts.pre {
		return updateOpts{}, fmt.Errorf("--version and --pre are mutually exclusive (the pin already names a specific build)")
	}
	if opts.channel == "main" && opts.pre {
		return updateOpts{}, fmt.Errorf("--channel main and --pre are mutually exclusive (main is already a pre-release stream)")
	}
	return opts, nil
}

func printUpdateUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: cc-connect update [flags]

Flags:
  --channel <stable|main>   Release channel (default: stable)
                              stable: latest tagged release
                              main:   latest rolling build from main branch (unstable)
  --version <tag>           Install a specific tagged release (e.g. v1.2.3 or 1.2.3)
                              Mutually exclusive with --channel main and --pre.
  --pre, --beta             Include pre-releases when picking latest stable
                              Mutually exclusive with --channel main and --version.

Examples:
  cc-connect update                       # latest stable
  cc-connect update --pre                 # latest including pre-releases
  cc-connect update --channel main        # latest rolling main build
  cc-connect update --version v1.2.0      # pin to a specific tag`)
}

// resolveTargetRelease picks which release to install based on opts.
func resolveTargetRelease(opts updateOpts) (*githubRelease, error) {
	switch {
	case opts.pinVersion != "":
		return fetchReleaseByTag(opts.pinVersion)
	case opts.channel == "main":
		return fetchReleaseByTag(mainChannelTag)
	default:
		return fetchRelease(opts.pre)
	}
}

// releaseDisplayVersion picks the user-facing version label for a release.
// For the main channel the rolling tag is just "main", but the release Name
// (or asset names) carry the actual main-<sha> identifier we want to show.
func releaseDisplayVersion(rel *githubRelease, opts updateOpts) string {
	if rel == nil {
		return ""
	}
	if opts.channel == "main" {
		if v := extractMainVersionFromAssets(rel); v != "" {
			return v
		}
		if rel.Name != "" {
			return rel.Name
		}
	}
	return rel.TagName
}

// extractMainVersionFromAssets pulls the "main-<sha>" identifier out of an
// asset filename like "cc-connect-main-abc1234-darwin-arm64.tar.gz". Returns
// "" if no asset matches the pattern.
func extractMainVersionFromAssets(rel *githubRelease) string {
	const prefix = "cc-connect-"
	for _, a := range rel.Assets {
		name := a.Name
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		// rest looks like "main-abc1234-darwin-arm64.tar.gz" — split off the
		// trailing "-<goos>-<goarch>..." and keep the version prefix.
		idx := strings.Index(rest, "-")
		if idx < 0 || !strings.HasPrefix(rest, "main-") {
			continue
		}
		// Find the position where "-<goos>-" starts. We assume goos is one of
		// linux/darwin/windows; the version is everything before that.
		for _, goos := range []string{"-linux-", "-darwin-", "-windows-"} {
			if i := strings.Index(rest, goos); i > 0 {
				return rest[:i]
			}
		}
	}
	return ""
}

// selectAssetURLs scans a release's assets list for the binary and archive
// matching the given goos/goarch. Returns ("", "") if the release has no
// assets attached (callers should fall back to URL guessing).
func selectAssetURLs(rel *githubRelease, goos, goarch string) (binURL, archiveURL string) {
	if rel == nil || len(rel.Assets) == 0 {
		return "", ""
	}

	archiveExt := ".tar.gz"
	binExt := ""
	if goos == "windows" {
		archiveExt = ".zip"
		binExt = ".exe"
	}

	archiveSuffix := fmt.Sprintf("-%s-%s%s", goos, goarch, archiveExt)
	binSuffix := fmt.Sprintf("-%s-%s%s", goos, goarch, binExt)

	for _, a := range rel.Assets {
		switch {
		case strings.HasSuffix(a.Name, archiveSuffix):
			archiveURL = a.BrowserDownloadURL
		case binExt == "" && strings.HasSuffix(a.Name, binSuffix) &&
			!strings.HasSuffix(a.Name, ".tar.gz") && !strings.HasSuffix(a.Name, ".zip"):
			binURL = a.BrowserDownloadURL
		case binExt != "" && strings.HasSuffix(a.Name, binSuffix):
			binURL = a.BrowserDownloadURL
		}
	}
	return binURL, archiveURL
}

// normalizeVersionTag accepts "1.2.3" or "v1.2.3" and returns the canonical
// "v1.2.3" form. Empty input returns empty. Non-semver inputs are returned
// unchanged so callers can pass through arbitrary tag names.
func normalizeVersionTag(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "v") {
		return s
	}
	if s[0] >= '0' && s[0] <= '9' {
		return "v" + s
	}
	return s
}

// fetchReleaseByTag fetches a specific release by tag name. Returns a clean
// "release not found" error on 404 so the caller can give a better message.
func fetchReleaseByTag(tag string) (*githubRelease, error) {
	url := githubTagAPI + "/" + tag
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("release not found: %s", tag)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned HTTP %d for tag %s", resp.StatusCode, tag)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("parse release: %w", err)
	}
	return &release, nil
}

// fetchRelease returns the latest release. If pre=true, includes pre-releases.
func fetchRelease(pre bool) (*githubRelease, error) {
	if pre {
		return fetchLatestPreRelease()
	}
	return fetchLatestStableRelease()
}

// fetchLatestPreRelease fetches the newest release (including pre-releases) from GitHub.
func fetchLatestPreRelease() (*githubRelease, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest("GET", githubAllAPI+"?per_page=10", nil)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}

	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("parse releases: %w", err)
	}

	if len(releases) == 0 {
		return nil, fmt.Errorf("no releases found")
	}

	// Return the first (newest) release, which may be a pre-release
	return &releases[0], nil
}

// fetchLatestStableRelease fetches the latest stable release (no pre-releases).
func fetchLatestStableRelease() (*githubRelease, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest("GET", githubAPI, nil)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			var release githubRelease
			if err := json.NewDecoder(resp.Body).Decode(&release); err == nil {
				return &release, nil
			}
		}
	}

	// Fallback: follow redirect from /releases/latest to extract tag
	latestURL := "https://github.com/" + githubRepo + "/releases/latest"
	noRedirect := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp2, err := noRedirect.Get(latestURL)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp2.Body.Close()

	loc := resp2.Header.Get("Location")
	if loc == "" {
		return nil, fmt.Errorf("no release found")
	}
	parts := strings.Split(loc, "/tag/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("unexpected redirect: %s", loc)
	}
	return &githubRelease{TagName: parts[1], HTMLURL: loc}, nil
}

func binaryAssetName(tag string) string {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	name := fmt.Sprintf("cc-connect-%s-%s-%s", tag, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func archiveAssetName(tag string) string {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	base := fmt.Sprintf("cc-connect-%s-%s-%s", tag, goos, goarch)
	if goos == "windows" {
		return base + ".zip"
	}
	return base + ".tar.gz"
}

// extractBinaryFromArchive extracts the cc-connect binary from a .tar.gz or .zip archive.
func extractBinaryFromArchive(archivePath, archiveName string) (string, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		return extractFromZip(archivePath)
	}
	return extractFromTarGz(archivePath)
}

func extractFromTarGz(archivePath string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if strings.HasPrefix(hdr.Name, "cc-connect") {
			tmp, err := os.CreateTemp("", "cc-connect-update-*")
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(tmp, tr); err != nil {
				tmp.Close()
				os.Remove(tmp.Name())
				return "", fmt.Errorf("extract: %w", err)
			}
			tmp.Close()
			return tmp.Name(), nil
		}
	}
	return "", fmt.Errorf("binary not found in archive")
}

func extractFromZip(archivePath string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("zip: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		if !strings.HasPrefix(f.Name, "cc-connect") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		tmp, err := os.CreateTemp("", "cc-connect-update-*")
		if err != nil {
			rc.Close()
			return "", err
		}
		if _, err := io.Copy(tmp, rc); err != nil {
			tmp.Close()
			rc.Close()
			os.Remove(tmp.Name())
			return "", fmt.Errorf("extract: %w", err)
		}
		rc.Close()
		tmp.Close()
		return tmp.Name(), nil
	}
	return "", fmt.Errorf("binary not found in archive")
}

func downloadToTemp(url string) (string, error) {
	client := &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "cc-connect-update-*")
	if err != nil {
		return "", err
	}

	size, err := io.Copy(tmp, resp.Body)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write: %w", err)
	}
	tmp.Close()

	fmt.Printf("Downloaded %.1f MB\n", float64(size)/1024/1024)
	return tmp.Name(), nil
}

func replaceExecutable(target, src string) error {
	if err := os.Chmod(src, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// On Windows, rename over a running exe is not possible directly.
	// Move old binary aside, then move new one in.
	backup := target + ".old"
	os.Remove(backup)

	if err := os.Rename(target, backup); err != nil {
		return fmt.Errorf("backup old binary: %w", err)
	}

	if err := copyFile(src, target); err != nil {
		// Attempt to restore
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			slog.Warn("update: failed to restore old binary after copy error", "error", restoreErr)
		}
		return fmt.Errorf("install new binary: %w", err)
	}

	if err := os.Chmod(target, 0o755); err != nil {
		return fmt.Errorf("chmod new binary: %w", err)
	}

	os.Remove(backup)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func checkUpdate() {
	pre := false
	for _, arg := range os.Args[2:] {
		if arg == "--pre" || arg == "--beta" {
			pre = true
		}
	}

	release, err := fetchRelease(pre)
	if err != nil {
		return
	}
	if isNewer(release.TagName, version) {
		hint := "cc-connect update"
		if release.Prerelease {
			hint = "cc-connect update --pre"
		}
		fmt.Fprintf(os.Stderr, "Update available: %s → %s (run: %s)\n", version, release.TagName, hint)
	}
}

// isNewer returns true if latest represents a newer release than current.
// Handles semver tags (v1.2.3), pre-release tags (v1.2.3-beta.1, v1.2.3-rc.1),
// and dev builds (v1.2.3-10-gHASH).
func isNewer(latest, current string) bool {
	if latest == "" || current == "" {
		return false
	}
	if strings.HasPrefix(current, "dev") {
		return true
	}

	l := strings.TrimPrefix(latest, "v")
	c := strings.TrimPrefix(current, "v")

	lBase, lPre, _ := strings.Cut(l, "-")
	cBase, cPre, _ := strings.Cut(c, "-")

	lParts := strings.Split(lBase, ".")
	cParts := strings.Split(cBase, ".")

	for i := 0; i < len(lParts) || i < len(cParts); i++ {
		var lv, cv int
		if i < len(lParts) {
			_, _ = fmt.Sscanf(lParts[i], "%d", &lv)
		}
		if i < len(cParts) {
			_, _ = fmt.Sscanf(cParts[i], "%d", &cv)
		}
		if lv > cv {
			return true
		}
		if lv < cv {
			return false
		}
	}

	// Same base version — compare pre-release suffix
	// No pre-release beats a pre-release (1.2.0 > 1.2.0-beta.1)
	if cPre != "" && lPre == "" {
		return true
	}
	if cPre == "" && lPre != "" {
		return false
	}
	// Both have pre-release: split on "." and compare each segment
	// numerically where possible so beta.10 > beta.2.
	if lPre != "" && cPre != "" {
		return comparePreRelease(lPre, cPre) > 0
	}

	return false
}

// comparePreRelease compares two pre-release strings segment by segment.
// Numeric segments are compared as integers; non-numeric segments are
// compared lexicographically. Returns >0 if a is greater, <0 if b is
// greater, 0 if equal.
func comparePreRelease(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	max := len(aParts)
	if len(bParts) > max {
		max = len(bParts)
	}
	for i := 0; i < max; i++ {
		var ap, bp string
		if i < len(aParts) {
			ap = aParts[i]
		}
		if i < len(bParts) {
			bp = bParts[i]
		}

		var an, bn int
		aN, _ := fmt.Sscanf(ap, "%d", &an)
		bN, _ := fmt.Sscanf(bp, "%d", &bn)
		aIsNum := aN == 1 && fmt.Sprintf("%d", an) == ap
		bIsNum := bN == 1 && fmt.Sprintf("%d", bn) == bp

		if aIsNum && bIsNum {
			if an != bn {
				return an - bn
			}
			continue
		}
		// Non-numeric: lexicographic
		if ap < bp {
			return -1
		}
		if ap > bp {
			return 1
		}
	}
	return 0
}

// syncNpmPackageVersion detects if the binary lives inside an npm package
// (node_modules/cc-connect/bin/) and updates the package.json version to
// match the newly installed binary. Without this, the npm wrapper's run.js
// would see a version mismatch and re-download the old version on next run.
func syncNpmPackageVersion(execPath, newVer string) {
	binDir := filepath.Dir(execPath)
	if filepath.Base(binDir) != "bin" {
		return
	}
	pkgDir := filepath.Dir(binDir)
	pkgJSON := filepath.Join(pkgDir, "package.json")

	data, err := os.ReadFile(pkgJSON)
	if err != nil {
		return
	}

	var pkg map[string]any
	if err := json.Unmarshal(data, &pkg); err != nil {
		return
	}

	name, _ := pkg["name"].(string)
	if name != "cc-connect" {
		return
	}

	oldVer, _ := pkg["version"].(string)
	// Normalize both sides by stripping optional "v" prefix before comparing.
	// package.json may store "v1.0.0" while newVer is already stripped to "1.0.0".
	oldNorm := strings.TrimPrefix(oldVer, "v")
	newNorm := strings.TrimPrefix(newVer, "v")
	if oldNorm == newNorm {
		return
	}

	pkg["version"] = newVer
	out, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return
	}
	out = append(out, '\n')
	if err := os.WriteFile(pkgJSON, out, 0o644); err != nil {
		slog.Warn("update: failed to sync npm package.json version", "error", err)
		fmt.Println("⚠️  Note: npm package version not synced. If the next run re-downloads an old version,")
		fmt.Println("   please run: npm update -g cc-connect")
	} else {
		slog.Debug("update: synced npm package.json version", "old", oldVer, "new", newVer)
	}
}
