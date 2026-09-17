package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── Version comparison ──

func TestSemverLt(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.0.0", "1.0.1", true},      // patch newer
		{"1.0.1", "1.0.0", false},     // patch older -> not lt
		{"1.0.0", "1.0.0", false},     // equal
		{"1.0.0", "2.0.0", true},      // major newer
		{"2.0.0", "1.9.9", false},     // major older
		{"dev", "1.0.0", false},       // dev field non-numeric -> never older
		{"1.0.0", "dev", false},       // target non-numeric -> not older
		{"(devel)", "1.0.0", false},   // go build-info dev marker
		{"1.2.3-rc1", "1.2.3", false}, // pre-release non-numeric field -> not older
		{"1.0", "1.0.1", true},        // unequal lengths, padded: 1.0.0 < 1.0.1
		{"1.0.0", "1.0", false},       // unequal lengths, padded equal
		{"v1.0.0", "v1.0.1", true},    // leading v on both
		{"v1.2.0", "1.3.0", true},     // leading v on one only
		{"1.10.0", "1.9.0", false},    // numeric, not lexical (10 > 9)
		{"1.9.0", "1.10.0", true},     // numeric, not lexical
	}
	for _, c := range cases {
		if got := semverLt(c.a, c.b); got != c.want {
			t.Errorf("semverLt(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNormVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v1.2.3":   "1.2.3",
		" V1.2.3 ": "1.2.3",
		"1.2.3":    "1.2.3",
	} {
		if got := normVersion(in); got != want {
			t.Errorf("normVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── Asset naming ──

func TestAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "worklog-linux-amd64"},
		{"windows", "amd64", "worklog-windows-amd64.exe"},
		{"darwin", "arm64", "worklog-darwin-arm64"},
	}
	for _, c := range cases {
		if got := assetName(c.goos, c.goarch); got != c.want {
			t.Errorf("assetName(%q, %q) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

// ── resolvedVersion fallback ──

func TestResolveVersion(t *testing.T) {
	cases := []struct {
		name           string
		stamped, build string
		want           string
	}{
		{"stamped release wins", "v1.4.2", "v9.9.9", "v1.4.2"},
		{"dev falls back to build module version", "dev", "v0.4.2", "v0.4.2"},
		{"empty stamped falls back too", "", "v0.4.2", "v0.4.2"},
		{"dev with devel build stays dev", "dev", "(devel)", "dev"},
		{"dev with empty build stays dev", "dev", "", "dev"},
		{"empty stamped with empty build stays dev", "", "", "dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveVersion(c.stamped, c.build); got != c.want {
				t.Errorf("resolveVersion(%q, %q) = %q, want %q", c.stamped, c.build, got, c.want)
			}
		})
	}
}

func TestResolvedVersionUsesStamp(t *testing.T) {
	orig := version
	defer func() { version = orig }()
	version = "1.4.2"
	if got := resolvedVersion(); got != "1.4.2" {
		t.Errorf("resolvedVersion() = %q, want 1.4.2", got)
	}
}

// ── Atomic single-file swap ──

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestSwapFilePOSIX(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worklog")
	newf := filepath.Join(dir, "new")
	writeFile(t, target, "old")
	writeFile(t, newf, "new")

	if err := swapFileImpl(newf, target, false, os.Rename); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := readFile(t, target); got != "new" {
		t.Errorf("target = %q, want new", got)
	}
	if _, err := os.Stat(newf); !os.IsNotExist(err) {
		t.Errorf("new file should have been renamed away")
	}
}

func TestSwapFileWindowsRenameAside(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worklog.exe")
	newf := filepath.Join(dir, "new")
	writeFile(t, target, "old")
	writeFile(t, newf, "new")

	// Simulate the Windows branch (move aside + move in) using plain renames.
	if err := swapFileImpl(newf, target, true, os.Rename); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := readFile(t, target); got != "new" {
		t.Errorf("target = %q, want new", got)
	}
	aside := fmt.Sprintf("%s.old-%d", target, os.Getpid())
	if got := readFile(t, aside); got != "old" {
		t.Errorf("aside = %q, want old (the moved-aside original)", got)
	}
	// clearStale removes the leftover on a subsequent run.
	clearStale(dir, "worklog.exe")
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Errorf("clearStale did not remove the .old-* leftover")
	}
}

func TestClearStaleIsScopedToBinary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "worklog.old-1"), "ours")
	writeFile(t, filepath.Join(dir, "other.old-2"), "someone else's")
	clearStale(dir, "worklog")
	if _, err := os.Stat(filepath.Join(dir, "worklog.old-1")); !os.IsNotExist(err) {
		t.Errorf("our own .old-* leftover should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "other.old-2")); err != nil {
		t.Errorf("an unrelated *.old-* file must NOT be removed")
	}
}

func TestSwapFileWindowsRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worklog.exe")
	newf := filepath.Join(dir, "new")
	writeFile(t, target, "old")
	writeFile(t, newf, "new")

	// Fail the move-IN (new -> target) to exercise the rollback that restores the
	// aside original, so the install is never left without a runnable binary.
	failMoveIn := func(oldpath, newpath string) error {
		if oldpath == newf {
			return fmt.Errorf("simulated AV lock on move-in")
		}
		return os.Rename(oldpath, newpath)
	}
	err := swapFileImpl(newf, target, true, failMoveIn)
	if err == nil {
		t.Fatalf("expected an error from the failed move-in")
	}
	if got := readFile(t, target); got != "old" {
		t.Errorf("after rollback target = %q, want old (restored)", got)
	}
	if got := readFile(t, newf); got != "new" {
		t.Errorf("new file should still exist after a failed move-in, got %q", got)
	}
}

// ── Test helpers for seaming the network + spawn ──

// installSeams saves the update seams and restores them at test cleanup; callers
// override individual seam vars afterward as needed.
func installSeams(t *testing.T) {
	t.Helper()
	oFetch, oDownload, oResolve, oSpawn, oWritable, oCache := fetchLatestFn, downloadFn, resolveTargetFn, spawnDetachedFn, dirWritableFn, cacheDirFn
	t.Cleanup(func() {
		fetchLatestFn, downloadFn, resolveTargetFn, spawnDetachedFn, dirWritableFn, cacheDirFn = oFetch, oDownload, oResolve, oSpawn, oWritable, oCache
	})
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// relWith builds a release whose asset for THIS platform has the given url/digest.
func relWith(tag, url, digest string) *release {
	return &release{
		tag:    tag,
		assets: map[string]asset{assetName(runtime.GOOS, runtime.GOARCH): {url: url, digest: digest}},
	}
}

// ── doUpdate: checksum / no-asset / not-writable / success ──

func TestDoUpdateChecksumMismatch(t *testing.T) {
	installSeams(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "worklog")
	writeFile(t, target, "old")

	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })

	payload := []byte("the new binary")
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return relWith("v1.2.0", "http://example/asset", "deadbeef"), nil // wrong digest
	}
	downloadFn = func(ctx context.Context, c *http.Client, url, dest string) error {
		return os.WriteFile(dest, payload, 0o644)
	}
	resolveTargetFn = func() (string, error) { return target, nil }

	_, _, _, err := doUpdate(false, time.Second)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch error, got %v", err)
	}
	if got := readFile(t, target); got != "old" {
		t.Errorf("target must be untouched on checksum failure, got %q", got)
	}
}

func TestDoUpdateSuccess(t *testing.T) {
	installSeams(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "worklog")
	writeFile(t, target, "old")

	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })

	payload := []byte("the new binary bytes")
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return relWith("v1.2.0", "http://example/asset", hashOf(payload)), nil
	}
	downloadFn = func(ctx context.Context, c *http.Client, url, dest string) error {
		return os.WriteFile(dest, payload, 0o644)
	}
	resolveTargetFn = func() (string, error) { return target, nil }

	old, newv, changed, err := doUpdate(false, time.Second)
	if err != nil {
		t.Fatalf("doUpdate: %v", err)
	}
	if !changed || old != "1.0.0" || newv != "1.2.0" {
		t.Fatalf("got (%q -> %q changed=%v), want (1.0.0 -> 1.2.0 changed=true)", old, newv, changed)
	}
	if got := readFile(t, target); got != string(payload) {
		t.Errorf("target = %q, want the new payload", got)
	}
}

func TestDoUpdateAlreadyCurrent(t *testing.T) {
	installSeams(t)
	version = "1.2.0"
	t.Cleanup(func() { version = "dev" })
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return relWith("v1.2.0", "http://example/asset", "x"), nil
	}
	downloadFn = func(ctx context.Context, c *http.Client, url, dest string) error {
		t.Fatal("must not download when already current")
		return nil
	}
	old, _, changed, err := doUpdate(false, time.Second)
	if err != nil || changed || old != "1.2.0" {
		t.Fatalf("got (%q changed=%v err=%v), want (1.2.0 changed=false err=nil)", old, changed, err)
	}
}

func TestDoUpdateNoAsset(t *testing.T) {
	installSeams(t)
	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return &release{tag: "v1.2.0", assets: map[string]asset{"worklog-other-arch": {url: "u"}}}, nil
	}
	_, _, _, err := doUpdate(false, time.Second)
	if err == nil || !strings.Contains(err.Error(), "no prebuilt binary") {
		t.Fatalf("want no-prebuilt-binary error, got %v", err)
	}
}

func TestDoUpdateNotWritable(t *testing.T) {
	installSeams(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "worklog")
	writeFile(t, target, "old")

	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })

	payload := []byte("new")
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return relWith("v1.2.0", "http://example/asset", ""), nil // no digest, skip verify
	}
	downloadFn = func(ctx context.Context, c *http.Client, url, dest string) error {
		return os.WriteFile(dest, payload, 0o644)
	}
	resolveTargetFn = func() (string, error) { return target, nil }
	dirWritableFn = func(string) bool { return false }

	_, _, _, err := doUpdate(false, time.Second)
	if err == nil {
		t.Fatalf("want not-writable error")
	}
	var nwe *notWritableError
	if !errors.As(err, &nwe) {
		t.Fatalf("want *notWritableError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "sudo install -m 0755") {
		t.Errorf("error should carry the manual sudo install command: %v", err)
	}
	// The staged binary is kept for the manual install command.
	if _, statErr := os.Stat(nwe.tmp); statErr != nil {
		t.Errorf("staged tmp should be kept, stat: %v", statErr)
	}
	os.Remove(nwe.tmp)
	if got := readFile(t, target); got != "old" {
		t.Errorf("target must be untouched when not writable, got %q", got)
	}
}

func TestDoUpdateForceReinstall(t *testing.T) {
	installSeams(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "worklog")
	writeFile(t, target, "old")

	version = "1.2.0" // same as latest
	t.Cleanup(func() { version = "dev" })

	payload := []byte("reinstalled")
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return relWith("v1.2.0", "http://example/asset", hashOf(payload)), nil
	}
	downloadFn = func(ctx context.Context, c *http.Client, url, dest string) error {
		return os.WriteFile(dest, payload, 0o644)
	}
	resolveTargetFn = func() (string, error) { return target, nil }

	_, _, changed, err := doUpdate(true, time.Second) // force
	if err != nil || !changed {
		t.Fatalf("force should reinstall: changed=%v err=%v", changed, err)
	}
	if got := readFile(t, target); got != string(payload) {
		t.Errorf("target = %q, want reinstalled payload", got)
	}
}

// ── autoUpdate ──

// autoFixture wires an isolated cache dir + a writable target and records spawns.
type autoFixture struct {
	cacheDir string
	target   string
	spawned  [][]string
	spawnOK  bool
}

func newAutoFixture(t *testing.T, spawnOK bool) *autoFixture {
	t.Helper()
	installSeams(t)
	f := &autoFixture{cacheDir: t.TempDir(), spawnOK: spawnOK}
	f.target = filepath.Join(t.TempDir(), "worklog")
	writeFile(t, f.target, "old")
	cacheDirFn = func() string { return f.cacheDir }
	resolveTargetFn = func() (string, error) { return f.target, nil }
	downloadFn = func(ctx context.Context, c *http.Client, url, dest string) error {
		t.Fatal("auto path must never perform a synchronous download")
		return nil
	}
	spawnDetachedFn = func(args []string, logPath string) bool {
		f.spawned = append(f.spawned, args)
		return f.spawnOK
	}
	return f
}

func (f *autoFixture) lockPath() string { return filepath.Join(f.cacheDir, "auto-update.lock") }

func TestAutoUpdateDisabled(t *testing.T) {
	f := newAutoFixture(t, true)
	t.Setenv(autoUpdateEnv, "0")
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		t.Fatal("must not check the network when disabled")
		return nil, nil
	}
	autoUpdate()
	if len(f.spawned) != 0 {
		t.Errorf("disabled: must not spawn, got %v", f.spawned)
	}
}

func TestAutoUpdateAlreadyCurrent(t *testing.T) {
	f := newAutoFixture(t, true)
	version = "1.2.0"
	t.Cleanup(func() { version = "dev" })
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return relWith("v1.2.0", "u", "d"), nil
	}
	autoUpdate()
	if len(f.spawned) != 0 {
		t.Errorf("already current: must not spawn, got %v", f.spawned)
	}
}

func TestAutoUpdateSpawnsWhenNewer(t *testing.T) {
	f := newAutoFixture(t, true)
	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return relWith("v1.2.0", "u", "d"), nil
	}
	autoUpdate()
	if len(f.spawned) != 1 {
		t.Fatalf("newer: want exactly one spawn, got %v", f.spawned)
	}
	got := strings.Join(f.spawned[0], " ")
	if !strings.Contains(got, "update --release-lock") {
		t.Errorf("spawn args = %v, want update --release-lock <lock>", f.spawned[0])
	}
	// The lock is left held for the (mocked) detached child to release.
	if _, err := os.Stat(f.lockPath()); err != nil {
		t.Errorf("lock should be held after a successful spawn: %v", err)
	}
}

func TestAutoUpdateReleasesLockWhenSpawnFails(t *testing.T) {
	f := newAutoFixture(t, false) // spawn fails
	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return relWith("v1.2.0", "u", "d"), nil
	}
	autoUpdate()
	if len(f.spawned) != 1 {
		t.Fatalf("want one spawn attempt, got %v", f.spawned)
	}
	if _, err := os.Stat(f.lockPath()); !os.IsNotExist(err) {
		t.Errorf("lock must be released when the spawn fails")
	}
}

func TestAutoUpdateLockHeld(t *testing.T) {
	f := newAutoFixture(t, true)
	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return relWith("v1.2.0", "u", "d"), nil
	}
	// A fresh lock already held by "another session".
	if err := os.MkdirAll(f.cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.lockPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	autoUpdate()
	if len(f.spawned) != 0 {
		t.Errorf("lock held: must not spawn, got %v", f.spawned)
	}
}

func TestAutoUpdateOfflineSilent(t *testing.T) {
	f := newAutoFixture(t, true)
	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return nil, fmt.Errorf("dial tcp: i/o timeout")
	}
	autoUpdate() // must not panic or spawn
	if len(f.spawned) != 0 {
		t.Errorf("offline: must not spawn, got %v", f.spawned)
	}
}

func TestAutoUpdateUsesTTLCache(t *testing.T) {
	f := newAutoFixture(t, true)
	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })
	// Seed a fresh cache tag; fetch must NOT be called.
	if err := os.MkdirAll(f.cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCacheTagAt(t, filepath.Join(f.cacheDir, "update-check"), time.Now().Unix(), "v1.2.0")
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		t.Fatal("a fresh TTL cache must short-circuit the network check")
		return nil, nil
	}
	autoUpdate()
	if len(f.spawned) != 1 {
		t.Fatalf("cached newer tag should still spawn, got %v", f.spawned)
	}
}

func writeCacheTagAt(t *testing.T, path string, ts int64, tag string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d %s\n", ts, tag)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ── cmdUpdate exit-code contract ──

func TestCmdUpdateAutoAlwaysNil(t *testing.T) {
	_ = newAutoFixture(t, false)
	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return nil, fmt.Errorf("boom")
	}
	if err := cmdUpdate([]string{"--auto"}); err != nil {
		t.Fatalf("--auto must never return an error, got %v", err)
	}
}

func TestCmdUpdateReleasesLockOnError(t *testing.T) {
	installSeams(t)
	dir := t.TempDir()
	lock := filepath.Join(dir, "lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	// A failing fetch makes the sync run error; the --release-lock must still be
	// dropped on the way out.
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return nil, fmt.Errorf("offline")
	}
	err := cmdUpdate([]string{"--force", "--release-lock", lock})
	if err == nil {
		t.Fatalf("expected an error from the failed update")
	}
	if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
		t.Errorf("--release-lock must be released even when the update errors")
	}
}

// ── dirWritable real behavior ──

func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	if !dirWritable(dir) {
		t.Errorf("a fresh temp dir should be writable")
	}
	if dirWritable(filepath.Join(dir, "does-not-exist")) {
		t.Errorf("a non-existent dir should not be writable")
	}
}

// ── single-flight lock ──

func TestAcquireLockFreshHeldStale(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "lock")
	now := time.Now()

	if !acquireLock(lock, now) {
		t.Fatal("a fresh lock should be acquired")
	}
	if acquireLock(lock, now) {
		t.Error("a freshly-held lock must not be re-acquired")
	}
	// Age the lock past the stale threshold: it should now be reclaimed.
	old := now.Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if !acquireLock(lock, now) {
		t.Error("a stale lock should be reclaimed")
	}
}

func TestAcquireLockConcurrentSingleFlight(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "lock")
	now := time.Now()
	var wins int64
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if acquireLock(lock, now) {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("exactly one goroutine may hold the lock, got %d", wins)
	}
}

// ── auto-update throttle ──

func TestAutoUpdateThrottlesRepeatSpawnForSameTag(t *testing.T) {
	f := newAutoFixture(t, true)
	version = "1.0.0"
	t.Cleanup(func() { version = "dev" })
	fetchLatestFn = func(ctx context.Context, c *http.Client) (*release, error) {
		return relWith("v1.2.0", "u", "d"), nil
	}
	autoUpdate()
	if len(f.spawned) != 1 {
		t.Fatalf("first run should spawn once, got %v", f.spawned)
	}
	// Simulate the detached child finishing (it releases the lock). A second
	// session for the SAME tag must not re-spawn — the per-tag throttle holds.
	os.Remove(f.lockPath())
	autoUpdate()
	if len(f.spawned) != 1 {
		t.Errorf("same tag within TTL must not re-spawn, got %d spawns", len(f.spawned))
	}
}

// ── real detached spawn (exercises SysProcAttr / Release / log fd) ──

func TestSpawnDetachedExeRuns(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available to exercise the real detach path")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	logPath := filepath.Join(dir, "log")
	if !spawnDetachedExe(sh, []string{"-c", "printf done > '" + marker + "'"}, logPath) {
		t.Fatal("spawnDetachedExe returned false")
	}
	// The child is detached (we do not Wait): poll briefly for its effect.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, rerr := os.ReadFile(marker); rerr == nil && string(b) == "done" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("detached child did not run (marker never written)")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
