package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// checkInterval bounds how often the network is touched for a version check.
const checkInterval = 24 * time.Hour

// checkTimeout keeps a slow or unreachable network from delaying a command whose
// real job is to print one line.
const checkTimeout = 3 * time.Second

type cache struct {
	CheckedAt time.Time `json:"checkedAt"`
	LatestTag string    `json:"latestTag"`
}

func cachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "asbutler", "update-check.json")
}

func readCache() (cache, bool) {
	p := cachePath()
	if p == "" {
		return cache{}, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return cache{}, false
	}
	var c cache
	if json.Unmarshal(data, &c) != nil {
		return cache{}, false
	}
	return c, true
}

func writeCache(c cache) {
	p := cachePath()
	if p == "" {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	if data, err := json.Marshal(c); err == nil {
		_ = os.WriteFile(p, data, 0o644)
	}
}

// Available reports a newer release than current, or ok=false when there is
// nothing to say. Every failure — offline, no credentials, a malformed tag — is
// silent: this is a courtesy line, never a reason for a command to fail.
//
// Only commands whose output a human reads should call this. `list` must not:
// its output is parsed by other tools, and a network call has no place there.
func Available(current string) (rel Release, ok bool) {
	if os.Getenv("ASBUTLER_NO_UPDATE_CHECK") != "" {
		return Release{}, false
	}
	cur, err := ParseVersion(current)
	if err != nil {
		return Release{}, false
	}

	if c, found := readCache(); found && time.Since(c.CheckedAt) < checkInterval {
		return decide(cur, c.LatestTag)
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	latest, err := LatestRelease(ctx)
	if err != nil {
		// Record the attempt so an unreachable network is not retried every run.
		writeCache(cache{CheckedAt: time.Now()})
		return Release{}, false
	}
	writeCache(cache{CheckedAt: time.Now(), LatestTag: latest.Tag})
	return decide(cur, latest.Tag)
}

func decide(cur Version, tag string) (Release, bool) {
	if tag == "" {
		return Release{}, false
	}
	latest, err := ParseVersion(tag)
	if err != nil || !latest.Newer(cur) {
		return Release{}, false
	}
	return Release{Tag: tag, URL: releaseURL(tag)}, true
}
