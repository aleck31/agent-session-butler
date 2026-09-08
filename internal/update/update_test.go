package update

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	for _, s := range []string{"0.7.3", "v0.7.3", " v0.7.3 ", "1.0.0", "10.20.30"} {
		if _, err := ParseVersion(s); err != nil {
			t.Errorf("ParseVersion(%q): %v", s, err)
		}
	}
	// A tag that is not X.Y.Z must fail rather than being guessed at, or a
	// malformed release could read as an upgrade.
	for _, s := range []string{"", "v1", "1.2", "1.2.3.4", "1.2.x", "abc", "v1.2.-3", "1.2.3-rc1"} {
		if v, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) = %v, want an error", s, v)
		}
	}
}

func TestVersionNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.7.4", "0.7.3", true},
		{"0.8.0", "0.7.9", true},
		{"1.0.0", "0.99.99", true},
		{"0.7.3", "0.7.3", false},
		{"0.7.3", "0.7.4", false},
		{"0.7.9", "0.8.0", false},
		// Numeric, not lexical: 10 beats 9.
		{"0.10.0", "0.9.0", true},
		{"0.9.0", "0.10.0", false},
	} {
		a, _ := ParseVersion(tc.a)
		b, _ := ParseVersion(tc.b)
		if got := a.Newer(b); got != tc.want {
			t.Errorf("%s.Newer(%s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// The asset name has to match what the release workflow produces and install.sh
// looks for; a mismatch means update silently cannot find its download.
func TestAssetName(t *testing.T) {
	got, err := AssetName("v0.7.4")
	if err != nil {
		t.Fatalf("AssetName: %v", err)
	}
	want := "asbutler-v0.7.4-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if got != want {
		t.Errorf("AssetName = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "asbutler-v0.7.4-") {
		t.Errorf("AssetName = %q, must start with asbutler-<tag>-", got)
	}
}

// ASBUTLER_NO_UPDATE_CHECK must suppress the check entirely — scripts and CI
// should not be making network calls because they printed a version.
func TestAvailableRespectsTheOptOut(t *testing.T) {
	t.Setenv("ASBUTLER_NO_UPDATE_CHECK", "1")
	if _, ok := Available("0.0.1"); ok {
		t.Error("the check ran despite ASBUTLER_NO_UPDATE_CHECK")
	}
}

// A build whose own version is unparseable cannot be compared, so it must say
// nothing rather than guess.
func TestAvailableIgnoresAnUnparseableCurrentVersion(t *testing.T) {
	t.Setenv("ASBUTLER_NO_UPDATE_CHECK", "")
	if _, ok := Available("not-a-version"); ok {
		t.Error("expected no notice for an unparseable current version")
	}
}

// A fresh cache entry is used instead of the network, so repeated invocations
// cost nothing.
func TestAvailableUsesAFreshCache(t *testing.T) {
	sandboxCache(t)
	t.Setenv("ASBUTLER_NO_UPDATE_CHECK", "")
	writeCache(cache{CheckedAt: time.Now(), LatestTag: "v9.9.9"})

	rel, ok := Available("0.7.3")
	if !ok {
		t.Fatal("expected a notice from the cached tag")
	}
	if rel.Tag != "v9.9.9" {
		t.Errorf("tag: got %q, want the cached v9.9.9", rel.Tag)
	}
	if !strings.Contains(rel.URL, "v9.9.9") {
		t.Errorf("url %q should point at the release", rel.URL)
	}
}

// An older-or-equal cached tag is not a notice.
func TestAvailableSaysNothingWhenCurrent(t *testing.T) {
	sandboxCache(t)
	t.Setenv("ASBUTLER_NO_UPDATE_CHECK", "")
	for _, tag := range []string{"v0.7.3", "v0.7.2", "", "garbage"} {
		writeCache(cache{CheckedAt: time.Now(), LatestTag: tag})
		if rel, ok := Available("0.7.3"); ok {
			t.Errorf("cached tag %q produced a notice for %v", tag, rel)
		}
	}
}

// A failed check records the attempt, so an unreachable network is not retried on
// every single invocation.
func TestFailedCheckIsCachedToAvoidRetrying(t *testing.T) {
	sandboxCache(t)
	writeCache(cache{CheckedAt: time.Now()}) // empty tag = the failure marker
	c, ok := readCache()
	if !ok || c.LatestTag != "" || c.CheckedAt.IsZero() {
		t.Fatalf("cache round-trip lost the failure marker: %+v", c)
	}
	// A recorded failure yields no notice, and no network call is needed to know.
	t.Setenv("ASBUTLER_NO_UPDATE_CHECK", "")
	if _, ok := Available("0.0.1"); ok {
		t.Error("a failure marker should not produce a notice")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	sandboxCache(t)
	if _, ok := readCache(); ok {
		t.Fatal("a fresh cache should be absent")
	}
	want := cache{CheckedAt: time.Now().Truncate(time.Second), LatestTag: "v1.2.3"}
	writeCache(want)
	got, ok := readCache()
	if !ok {
		t.Fatal("cache was not written")
	}
	if got.LatestTag != want.LatestTag || !got.CheckedAt.Equal(want.CheckedAt) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A corrupt cache file must not break the command; it just means no notice.
func TestCorruptCacheIsIgnored(t *testing.T) {
	sandboxCache(t)
	p := cachePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCache(); ok {
		t.Error("a corrupt cache should read as absent")
	}
}

// sandboxCache points os.UserCacheDir() at a temp directory. On darwin that is
// derived from HOME, on linux from XDG_CACHE_HOME, so both are set.
func sandboxCache(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("LocalAppData", filepath.Join(dir, "cache")) // windows
}
