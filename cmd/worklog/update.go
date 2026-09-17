// Self-update for the worklog binary. `worklog update` replaces the on-PATH
// binary with the latest GitHub release, atomically and in place, using only the
// standard library (no CGO, no extra module dependencies).
//
// Subcommand surface (`worklog update [FLAGS]`):
//
//	(no flags)   Check the latest release and, if newer, download + verify +
//	             atomically replace the binary in place. Reports old -> new or
//	             that it is already current.
//	--check      Report whether a newer release exists; change nothing.
//	--auto       The background path a SessionStart hook calls: TTL-gated,
//	             timeout-bounded, single-flight, and fully detached, so it never
//	             blocks or fails the caller. Opt out with WORKLOG_AUTO_UPDATE=0.
//	--force      Reinstall the latest release even when versions already match.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
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

// ── Constants ──

const (
	repo      = "MatLomax/worklog"
	apiLatest = "https://api.github.com/repos/%s/releases/latest"
	userAgent = "worklog-self-update"

	autoUpdateEnv = "WORKLOG_AUTO_UPDATE"
	checkTTLEnv   = "WORKLOG_UPDATE_CHECK_TTL"

	defaultTTLSeconds = 86_400 // a day
	httpTimeout       = 20 * time.Second
	autoHTTPTimeout   = 3 * time.Second
	lockStale         = time.Hour
)

// ── Release metadata ──

type asset struct {
	url    string
	digest string // hex sha256, "" when the release publishes none
}

type release struct {
	tag    string
	assets map[string]asset
}

// ── Seams (swapped out in tests) ──

// fetchLatestFn retrieves the latest release; overridden in tests to avoid the
// network. It returns an error on any network/parse failure so the sync path can
// report "offline" and the auto path can stay silent.
var fetchLatestFn = fetchLatest

// downloadFn writes the asset at url into dest; overridden in tests.
var downloadFn = downloadAsset

// resolveTargetFn locates the on-PATH binary to replace; overridden in tests.
var resolveTargetFn = resolveTarget

// spawnDetachedFn launches `worklog <args>` fully detached; overridden in tests.
var spawnDetachedFn = spawnDetached

// dirWritableFn reports whether dir can be written; overridden in tests.
var dirWritableFn = dirWritable

// cacheDirFn is the per-user cache directory holding the TTL cache and lock;
// overridden in tests.
var cacheDirFn = cacheDir

// ── Command dispatch ──

// cmdUpdate parses the `update` subcommand flags and dispatches. Errors returned
// here are printed by main() as "worklog: <err>" with exit 1; the --auto path
// always returns nil (exit 0), and any --release-lock is released on the way out.
func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "report whether a newer release exists; change nothing")
	auto := fs.Bool("auto", false, "background, TTL-gated path for a session-start hook")
	force := fs.Bool("force", false, "reinstall the latest release even when versions match")
	// Internal: a detached --auto child passes the lock dir to release when done.
	releaseLock := fs.String("release-lock", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *releaseLock != "" {
		defer os.Remove(*releaseLock)
	}
	switch {
	case *auto:
		autoUpdate()
		return nil // the auto path must never fail the caller
	case *check:
		return cmdUpdateCheck()
	default:
		return cmdUpdateRun(*force)
	}
}

func cmdUpdateRun(force bool) error {
	old, newv, changed, err := doUpdate(force, httpTimeout)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Printf("worklog is already up to date (%s)\n", old)
		return nil
	}
	fmt.Printf("worklog updated %s -> %s\n", old, newv)
	return nil
}

func cmdUpdateCheck() error {
	current := resolvedVersion()
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	rel, err := fetchLatestFn(ctx, &http.Client{Timeout: httpTimeout})
	if err != nil {
		return fmt.Errorf("could not reach the latest release (offline or GitHub unavailable): %v", err)
	}
	if semverLt(current, rel.tag) {
		fmt.Printf("worklog: a newer release is available (%s -> %s). Run `worklog update`.\n", current, normVersion(rel.tag))
	} else {
		fmt.Printf("worklog is up to date (%s)\n", current)
	}
	return nil
}

// ── Core update ──

// notWritableError reports that the target directory cannot be written, carrying
// the manual install command that finishes the update with elevated rights. We
// never escalate privileges ourselves.
type notWritableError struct {
	dir    string
	tmp    string
	target string
}

func (e *notWritableError) Error() string {
	return fmt.Sprintf("%s is not writable by you; the new binary was downloaded to %s. Install it manually:\n  sudo install -m 0755 %s %s",
		e.dir, e.tmp, e.tmp, e.target)
}

// doUpdate performs the update when a newer release exists (or force is set).
// It returns (old, new, changed, err): changed is false with a nil error when
// already current. Every failure is a clean error, never a panic.
func doUpdate(force bool, timeout time.Duration) (old, newv string, changed bool, err error) {
	current := resolvedVersion()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := &http.Client{Timeout: timeout}

	rel, ferr := fetchLatestFn(ctx, client)
	if ferr != nil {
		return "", "", false, fmt.Errorf("could not reach the latest release (offline or GitHub unavailable): %v", ferr)
	}
	proceed, tag, a, rerr := resolveUpdate(force, rel, current)
	if rerr != nil {
		return "", "", false, rerr
	}
	if !proceed {
		return current, "", false, nil
	}

	target, terr := resolveTargetFn()
	if terr != nil {
		return "", "", false, fmt.Errorf("could not locate the running binary: %v", terr)
	}
	dir := filepath.Dir(target)

	// Stage the download on the target's own filesystem so the final swap is a
	// rename, never a cross-device copy. If the target dir is not writable, stage
	// in the system temp dir instead and hand back a manual install command.
	writable := dirWritableFn(dir)
	stageDir := dir
	if !writable {
		stageDir = os.TempDir()
	}
	tmpf, cerr := os.CreateTemp(stageDir, ".worklog-dl-")
	if cerr != nil {
		return "", "", false, fmt.Errorf("could not create a temporary file in %s: %v", stageDir, cerr)
	}
	tmpPath := tmpf.Name()
	tmpf.Close()
	keep := false
	defer func() {
		if !keep {
			os.Remove(tmpPath)
		}
	}()

	if derr := downloadFn(ctx, client, a.url, tmpPath); derr != nil {
		return "", "", false, fmt.Errorf("download failed: %v", derr)
	}
	name := assetName(runtime.GOOS, runtime.GOARCH)
	if a.digest != "" {
		sum, herr := sha256File(tmpPath)
		if herr != nil {
			return "", "", false, fmt.Errorf("could not hash the download: %v", herr)
		}
		if sum != a.digest {
			return "", "", false, fmt.Errorf("checksum mismatch for %s %s: expected %s, got %s", name, tag, a.digest, sum)
		}
	} else {
		fmt.Fprintf(os.Stderr, "worklog: release %s publishes no checksum for %s; relying on HTTPS transport security.\n", tag, name)
	}
	if runtime.GOOS != "windows" {
		os.Chmod(tmpPath, 0o755)
	}

	if !writable {
		keep = true // leave the staged binary for the manual install command
		return "", "", false, &notWritableError{dir: dir, tmp: tmpPath, target: target}
	}

	if aerr := applyReplace(tmpPath, target); aerr != nil {
		// Keep the downloaded binary so a failed swap is always recoverable: the
		// user can install it over the target manually rather than being left
		// without a binary.
		keep = true
		return "", "", false, fmt.Errorf("could not apply the update: %v; the new binary is at %s — install it over %s to finish", aerr, tmpPath, target)
	}
	keep = true // consumed by the rename in applyReplace
	return current, normVersion(tag), true, nil
}

// resolveUpdate decides whether to proceed. It returns proceed=false with a nil
// error when already current, and a clean error when no asset exists for this
// platform.
func resolveUpdate(force bool, rel *release, current string) (bool, string, asset, error) {
	tag := rel.tag
	if !force && !semverLt(current, tag) {
		return false, tag, asset{}, nil
	}
	name := assetName(runtime.GOOS, runtime.GOARCH)
	a, ok := rel.assets[name]
	if !ok || a.url == "" {
		return false, tag, asset{}, fmt.Errorf(
			"no prebuilt binary for %s/%s; build from source: go install github.com/MatLomax/worklog/cmd/worklog@latest",
			runtime.GOOS, runtime.GOARCH)
	}
	return true, tag, a, nil
}

// ── Automatic (session-start) path ──

// autoUpdate is the SessionStart path: it never blocks, never hangs, and never
// fails the caller. It TTL-caches the last-seen tag, bounds the network check by
// a hard wall-clock deadline, single-flights via an atomic mkdir lock, and
// launches the actual download in a detached background process before returning.
func autoUpdate() {
	// The session-start path must never crash the caller: guarantee exit 0
	// structurally by swallowing any panic, not just by auditing for none.
	defer func() { _ = recover() }()
	if autoUpdateDisabled() {
		return
	}
	current := resolvedVersion()
	now := time.Now()
	cdir := cacheDirFn()
	cacheFile := filepath.Join(cdir, "update-check")

	tag := readCacheTag(cacheFile, now)
	if tag == "" {
		// A socket timeout alone does not cover DNS resolution, so the check is
		// bounded by BOTH the client Timeout and a context deadline; on overrun
		// we act as if offline. This is sufficient in Go: net/http honours the
		// context deadline across dial (incl. resolution), connect, and read.
		ctx, cancel := context.WithTimeout(context.Background(), autoHTTPTimeout)
		defer cancel()
		rel, err := fetchLatestFn(ctx, &http.Client{Timeout: autoHTTPTimeout})
		if err != nil || rel == nil {
			return // offline, unreachable, or slow: stay silent, retry next session
		}
		tag = rel.tag
		writeCacheTag(cacheFile, now, tag)
	}

	if !semverLt(current, tag) {
		return // already current
	}

	target, err := resolveTargetFn()
	if err != nil {
		return
	}
	dir := filepath.Dir(target)
	if !dirWritableFn(dir) {
		fmt.Printf("worklog: a newer release is available (%s -> %s), but %s is not writable. Run `worklog update` with write access.\n",
			current, normVersion(tag), dir)
		return
	}

	// Throttle: at most one background attempt per tag per TTL. Without this, a
	// platform with no published asset (or a persistently failing update) would
	// spawn a doomed child and print a line on *every* session start.
	attempted := filepath.Join(cdir, "auto-update.attempted")
	if readCacheTag(attempted, now) == tag {
		return
	}

	// Single-flight: one background update across concurrent session starts.
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		return
	}
	lock := filepath.Join(cdir, "auto-update.lock")
	if !acquireLock(lock, now) {
		return
	}

	// The detached child runs the synchronous update, then drops the lock via the
	// --release-lock deferred cleanup. This path itself performs no download.
	if spawnDetachedFn([]string{"update", "--release-lock", lock}, filepath.Join(cdir, "auto-update.log")) {
		writeCacheTag(attempted, now, tag)
		fmt.Printf("worklog: updating %s -> %s in the background ...\n", current, normVersion(tag))
	} else {
		// The background update never started: release the lock so the next
		// session retries, and print nothing (no false "updating" impression).
		os.Remove(lock)
	}
}

// acquireLock atomically claims the single-flight lock via mkdir, reclaiming a
// stale lock left by a crashed run older than lockStale.
func acquireLock(lock string, now time.Time) bool {
	if err := os.Mkdir(lock, 0o755); err == nil {
		return true
	} else if !os.IsExist(err) {
		return false
	}
	info, err := os.Stat(lock)
	if err != nil {
		return false
	}
	if now.Sub(info.ModTime()) < lockStale {
		return false // another session is already updating
	}
	// Reclaim a crashed run's stale lock atomically: rename it aside under a
	// pid-unique name. Renaming the lock is a single operation exactly one racer
	// can win (the rest see it already gone), so concurrent reclaimers cannot both
	// end up holding the lock.
	aside := fmt.Sprintf("%s.stale-%d", lock, os.Getpid())
	if os.Rename(lock, aside) != nil {
		return false // another session won the steal, or it vanished
	}
	os.RemoveAll(aside)
	return os.Mkdir(lock, 0o755) == nil
}

func readCacheTag(path string, now time.Time) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return ""
	}
	ts, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return ""
	}
	if now.Unix()-ts < ttlSeconds() {
		return fields[1]
	}
	return ""
}

func writeCacheTag(path string, now time.Time, tag string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d %s\n", now.Unix(), tag)), 0o644)
}

func ttlSeconds() int64 {
	if v := os.Getenv(checkTTLEnv); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultTTLSeconds
}

func autoUpdateDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(autoUpdateEnv))) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

// ── Atomic single-file swap ──

// applyReplace clears any stale leftover, then swaps the staged binary onto the
// target. The staged file is already chmod'd and on the target's filesystem.
func applyReplace(newPath, target string) error {
	clearStale(filepath.Dir(target), filepath.Base(target))
	return swapFileImpl(newPath, target, runtime.GOOS == "windows", os.Rename)
}

// swapFileImpl moves newPath onto target, tolerating a running executable. POSIX
// replaces a running binary directly (the open inode survives). Windows refuses
// to overwrite a running .exe but allows renaming it, so the current file is
// moved aside first; if the move-in then fails (an AV lock or sharing violation),
// the original is moved back. Should even that rollback fail, the error names the
// aside file so recovery is possible, and doUpdate keeps the downloaded binary.
// The windows flag and rename primitive are parameters so the branch is
// unit-testable on any platform.
func swapFileImpl(newPath, target string, windows bool, rename func(oldpath, newpath string) error) error {
	if windows {
		if _, err := os.Stat(target); err == nil {
			aside := fmt.Sprintf("%s.old-%d", target, os.Getpid())
			if err := rename(target, aside); err != nil {
				return err
			}
			if err := rename(newPath, target); err != nil {
				if rbErr := rename(aside, target); rbErr != nil {
					return fmt.Errorf("could not move the new binary into place (%v) and could not restore the previous one from %s (%v)", err, aside, rbErr)
				}
				return err
			}
			return nil
		}
	}
	return rename(newPath, target)
}

// clearStale removes this binary's `*.old-*` aside leftovers that a prior Windows
// swap could not delete while its file was still locked. Scoped to the binary's
// own name so unrelated files in the directory are never touched.
func clearStale(dir, binName string) {
	matches, err := filepath.Glob(filepath.Join(dir, binName+".old-*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		os.RemoveAll(m)
	}
}

// ── Platform / asset resolution ──

// assetName is the release asset file name for a GOOS/GOARCH pair, e.g.
// worklog-linux-amd64 or worklog-windows-amd64.exe.
func assetName(goos, goarch string) string {
	name := fmt.Sprintf("worklog-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// resolveTarget is the real on-PATH binary, following any symlink so a
// ~/.local/bin/worklog symlink resolves to the file it points at.
func resolveTarget() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".worklog-wtest-")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func cacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "worklog")
}

// ── Version comparison ──

func normVersion(v string) string {
	return strings.TrimLeft(strings.TrimSpace(v), "vV")
}

// semverLt reports whether version a is strictly older than b (leading "v"
// optional, unequal field counts padded with zeros). A non-numeric field in
// either version yields "not older", so a dev build or a pre-release is never
// treated as upgradable — self-update only ever moves a real release forward.
func semverLt(a, b string) bool {
	fa := strings.Split(normVersion(a), ".")
	fb := strings.Split(normVersion(b), ".")
	n := len(fa)
	if len(fb) > n {
		n = len(fb)
	}
	for i := 0; i < n; i++ {
		xs, ys := "0", "0"
		if i < len(fa) {
			xs = fa[i]
		}
		if i < len(fb) {
			ys = fb[i]
		}
		x, xerr := strconv.Atoi(xs)
		y, yerr := strconv.Atoi(ys)
		if xerr != nil || yerr != nil {
			return false
		}
		if x < y {
			return true
		}
		if x > y {
			return false
		}
	}
	return false
}

// ── Network ──

func fetchLatest(ctx context.Context, client *http.Client) (*release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(apiLatest, repo), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	var raw struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if raw.TagName == "" {
		return nil, fmt.Errorf("release JSON has no tag_name")
	}
	rel := &release{tag: raw.TagName, assets: make(map[string]asset, len(raw.Assets))}
	for _, a := range raw.Assets {
		if a.Name == "" {
			continue
		}
		digest := ""
		if strings.HasPrefix(a.Digest, "sha256:") {
			digest = strings.TrimPrefix(a.Digest, "sha256:")
		}
		rel.assets[a.Name] = asset{url: a.URL, digest: digest}
	}
	return rel, nil
}

func downloadAsset(ctx context.Context, client *http.Client, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ── Detached spawn ──

// spawnDetached launches `worklog <args>` fully detached (its own session/process
// group, stdio to a log file), returning whether it started. The parent returns
// immediately; the child outlives it.
func spawnDetached(args []string, logPath string) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return spawnDetachedExe(exe, args, logPath)
}

// spawnDetachedExe is the injectable core of spawnDetached: it launches exe with
// the given args in its own session/process group, stdio to a log file, and
// releases the child so the parent never waits on it. Split out so a test can
// drive the real detach path against a harmless executable.
func spawnDetachedExe(exe string, args []string, logPath string) bool {
	cmd := exec.Command(exe, args...)
	if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
		cmd.Stdout = f
		cmd.Stderr = f
		defer f.Close()
	}
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return false
	}
	_ = cmd.Process.Release() // detach: do not wait, let it run on its own
	return true
}
