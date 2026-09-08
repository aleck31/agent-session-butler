// Package update checks for and installs newer releases. Downloads prefer the
// GitHub CLI (credentials, needed while the repo is private) and the new binary
// is renamed into place, never written over. See ADR-0004.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is the source of releases.
const Repo = "aleck31/agent-session-butler"

// Release is the part of a GitHub release this package needs.
type Release struct {
	Tag string // e.g. "v0.7.4"
	URL string // the release page, for pointing a user at the notes
}

// Version is a parsed X.Y.Z. Anything else fails to parse rather than being
// guessed at, so a malformed tag can never look like an upgrade.
type Version [3]int

func ParseVersion(s string) (Version, error) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("not X.Y.Z: %q", s)
	}
	var v Version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("not X.Y.Z: %q", s)
		}
		v[i] = n
	}
	return v, nil
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// Newer reports whether v is strictly newer than other.
func (v Version) Newer(other Version) bool {
	for i := range v {
		if v[i] != other[i] {
			return v[i] > other[i]
		}
	}
	return false
}

// LatestRelease resolves the newest release, trying gh before an anonymous fetch.
func LatestRelease(ctx context.Context) (Release, error) {
	if tag, err := latestViaGH(ctx); err == nil && tag != "" {
		return Release{Tag: tag, URL: releaseURL(tag)}, nil
	}
	tag, err := latestViaAPI(ctx)
	if err != nil {
		return Release{}, err
	}
	return Release{Tag: tag, URL: releaseURL(tag)}, nil
}

func releaseURL(tag string) string {
	return "https://github.com/" + Repo + "/releases/tag/" + tag
}

func latestViaGH(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, "gh", "release", "view",
		"--repo", Repo, "--json", "tagName", "--jq", ".tagName").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func latestViaAPI(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 here is usually a private repo answering an anonymous request.
		return "", fmt.Errorf("github returned %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", errors.New("release has no tag_name")
	}
	return body.TagName, nil
}

// AssetName is the release asset for this machine, matching what the release
// workflow produces and install.sh looks for.
func AssetName(tag string) (string, error) {
	var goos, arch string
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		goos = runtime.GOOS
	default:
		return "", fmt.Errorf("unsupported OS %q", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
		arch = runtime.GOARCH
	default:
		return "", fmt.Errorf("unsupported architecture %q", runtime.GOARCH)
	}
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("asbutler-%s-%s-%s%s", tag, goos, arch, ext), nil
}

// Apply downloads this machine's asset for tag and replaces the running
// executable with it, returning the path that was replaced.
func Apply(ctx context.Context, tag string) (string, error) {
	asset, err := AssetName(tag)
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Resolve symlinks so a linked install is replaced at its real location.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	// Staged in the target's directory because the final rename must stay within
	// one filesystem to be atomic.
	stage := filepath.Join(filepath.Dir(exe), ".asbutler-update-"+strconv.Itoa(os.Getpid()))
	defer os.Remove(stage) // a no-op once the rename has consumed it

	if err := download(ctx, tag, asset, stage); err != nil {
		return "", err
	}
	if err := verifyBinary(stage, tag); err != nil {
		return "", err
	}
	mode := os.FileMode(0o755)
	if info, err := os.Stat(exe); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(stage, mode); err != nil {
		return "", err
	}
	// Rename, never write in place: macOS caches a code signature per inode and
	// SIGKILLs a binary whose bytes changed underneath it (ADR-0004).
	if err := os.Rename(stage, exe); err != nil {
		return "", fmt.Errorf("could not replace %s: %w", exe, err)
	}
	return exe, nil
}

func download(ctx context.Context, tag, asset, dest string) error {
	if _, err := exec.LookPath("gh"); err == nil {
		cmd := exec.CommandContext(ctx, "gh", "release", "download", tag,
			"--repo", Repo, "--pattern", asset, "--output", dest, "--clobber")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		// Keep gh's own message: it separates "not logged in" from "no such asset".
		ghErr := lastLine(strings.TrimSpace(string(out)))
		if e := downloadHTTP(ctx, tag, asset, dest); e != nil {
			return fmt.Errorf("gh: %s; anonymous download: %w", ghErr, e)
		}
		return nil
	}
	return downloadHTTP(ctx, tag, asset, dest)
}

func downloadHTTP(ctx context.Context, tag, asset, dest string) error {
	url := "https://github.com/" + Repo + "/releases/download/" + tag + "/" + asset
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: %s (a private repo needs `gh auth login`)", asset, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// verifyBinary runs the download's own `version` before trusting it, so a
// truncated or wrong-platform file fails here instead of replacing a working
// binary with a broken one.
func verifyBinary(path, tag string) error {
	if err := os.Chmod(path, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run: %w", err)
	}
	want := strings.TrimPrefix(tag, "v")
	if !strings.Contains(string(out), want) {
		return fmt.Errorf("the downloaded binary reports %q, expected %s",
			strings.TrimSpace(string(out)), want)
	}
	return nil
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(strings.TrimRight(s, "\n"), '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
