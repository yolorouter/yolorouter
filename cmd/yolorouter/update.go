package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/yolorouter/yolorouter/internal/config"
	"github.com/yolorouter/yolorouter/internal/version"
)

// githubAPITimeout bounds both the release-metadata lookup and each asset
// download. Update runs are interactive and user-initiated, so a stalled
// GitHub must fail the command within a bounded wait rather than hang.
const githubAPITimeout = 30 * time.Second

// binaryName is the executable filename inside a goreleaser-produced
// archive (set by .goreleaser.yaml's builds.binary). extractBinary looks for
// this basename regardless of any parent directory the archive wraps it in.
const binaryName = "yolorouter"

// runUpdate is the `yolorouter update` command: a semi-automatic upgrade
// that resolves the latest release, verifies its checksum, backs up the
// running binary, and atomically replaces it — then leaves the actual
// restart to the operator. It deliberately does NOT
// go through bootstrap.Init: an upgrade only needs config.update.*, never a
// live database connection (same shape as db:backup).
func runUpdate(ctx context.Context, args []string) error {
	flagSet, err := parseCommandFlags("update", args, 0, nil)
	if err != nil {
		return err
	}
	configPath := flagSet.Lookup("config").Value.String()
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	repo := version.ResolveRepo(cfg.Update.Enabled, cfg.Update.GitHubRepo)
	if repo == "" {
		return fmt.Errorf("update is disabled: set update.enabled=true and/or update.github_repo, or rebuild with DEFAULT_GITHUB_REPO")
	}

	client := &http.Client{Timeout: githubAPITimeout}
	// Asset downloads (binary archive + checksums.txt) can be tens of MB; the
	// 30s metadata timeout fails them on slower links. Use a longer timeout
	// for downloads while keeping the short metadata timeout.
	downloadClient := &http.Client{Timeout: 5 * time.Minute}

	// Windows can't atomically replace a running .exe (the file is locked),
	// so automatic update is unsupported there — point the user at the
	// release page instead of attempting a replacement that would fail
	// mid-way and leave no binary behind.
	if runtime.GOOS == "windows" {
		rel, err := fetchLatestReleaseWith(ctx, client, repo)
		if err != nil {
			return fmt.Errorf("automatic update is unsupported on windows, and the latest-release lookup also failed: %w", err)
		}
		fmt.Printf("automatic update is unsupported on windows\n")
		fmt.Printf("download the latest release (%s) manually from: %s\n", rel.TagName, rel.HTMLURL)
		return nil
	}

	current := version.Version
	if err := currentUpdatable(current); err != nil {
		return err
	}

	// Capture the executable's identity BEFORE any network lookup or download:
	// a concurrent updater or package manager that installs a newer binary
	// during the release lookup or asset download would otherwise be missed
	// by a post-download stat (it'd record the newer binary as the baseline,
	// the post-prompt re-stat would pass, and our stale downloaded bytes
	// would overwrite the newer install).
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	// os.Executable may return a symlink path on macOS (its contract permits
	// it); operating on the link would replace the symlink, not the real
	// target a service invokes, so the service would stay on the old binary
	// despite reported success. Resolve to the real path.
	if real, evalErr := filepath.EvalSymlinks(exe); evalErr == nil && real != "" {
		exe = real
	}
	initialInfo, err := os.Stat(exe)
	if err != nil {
		return fmt.Errorf("stat current executable: %w", err)
	}

	rel, err := fetchLatestReleaseWith(ctx, client, repo)
	if err != nil {
		return fmt.Errorf("look up latest release: %w", err)
	}

	// A non-semver latest tag (empty tag_name, or a release whose tag isn't
	// semver) makes Compare treat it as older than current, printing a
	// misleading "already up to date". Validate the way VersionService does,
	// surfacing a config/release error instead.
	if !semver.IsValid(rel.TagName) {
		return fmt.Errorf("latest release tag %q is not a valid semver; check update.github_repo / release source configuration", rel.TagName)
	}
	// Reject a prerelease latest (e.g. v1.3.0-rc1 published as
	// /releases/latest): installing it would make current a prerelease,
	// which currentUpdatable then refuses to advance from — the user gets
	// stuck on the RC. CE only updates to exact-tag stable releases.
	if semver.Prerelease(rel.TagName) != "" {
		return fmt.Errorf("latest release %q is a prerelease; the updater only installs stable tags. Pin to a stable release or wait for it", rel.TagName)
	}
	if semver.Compare(rel.TagName, current) <= 0 {
		fmt.Printf("already up to date (%s)\n", current)
		return nil
	}

	asset, err := matchAsset(rel.Assets, runtime.GOOS, runtime.GOARCH, rel.TagName)
	if err != nil {
		return err
	}
	checksumsAsset, err := findAsset(rel.Assets, "checksums.txt")
	if err != nil {
		return err
	}

	// The archive and checksums.txt are held in memory (downloadWith returns
	// []byte) and the staged binary is written beside the executable, so the
	// updater does not need a writable temp directory — allocating one would
	// make updates fail on systems where TMPDIR is read-only or absent even
	// though the actual writes never touch it.
	assetBytes, err := downloadWith(ctx, downloadClient, asset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}
	checksumsBytes, err := downloadWith(ctx, downloadClient, checksumsAsset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("download checksums.txt: %w", err)
	}

	// SHA256 verify BEFORE extracting or replacing: a tampered or truncated
	// download must never reach the binary path. A failed check is a hard
	// stop, never "replace anyway".
	if err := verifyChecksum(assetBytes, asset.Name, checksumsBytes); err != nil {
		return fmt.Errorf("checksum verification failed, not replacing: %w", err)
	}

	binaryBytes, err := extractBinary(assetBytes)
	if err != nil {
		return fmt.Errorf("extract binary from %s: %w", asset.Name, err)
	}
	// The archive's checksum is over the tar.gz, not the inner binary — an
	// independent magic-number check guards against a corrupt archive that
	// unpacked to something other than the expected executable.
	if !isExecutable(binaryBytes) {
		return fmt.Errorf("extracted binary from %s is not a recognized executable format", asset.Name)
	}

	fmt.Printf("Upgrade yolorouter %s -> %s?\n\n", current, rel.TagName)
	fmt.Println("WARNING: restarting after upgrade will run database migrations.")
	fmt.Println("For a full rollback, back up the database first:")
	fmt.Println(backupCommand(configPath))
	fmt.Println("The .bak binary backup only allows rollback BEFORE you restart.")
	fmt.Print("\nContinue? [y/N] ")
	var answer string
	_, _ = fmt.Scanln(&answer)
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		fmt.Println("upgrade cancelled")
		return nil
	}

	// Acquire an inter-process lock before revalidation + replacement: two
	// overlapping `update` invocations both pass a single re-stat against the
	// original binary, then race to replaceBinary — the loser would rename its
	// stale downloaded bytes over the newer binary the winner installed,
	// silently downgrading. The lock serializes revalidation+replace so the
	// second acquirer aborts.
	lockPath, lock, err := acquireUpdateLock(filepath.Dir(exe))
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close(); _ = os.Remove(lockPath) }()

	// Re-stat before replacing: if the executable changed during the prompt
	// (another update installed a newer binary), our downloaded bytes are now
	// stale — abort rather than downgrade.
	info, err := os.Stat(exe)
	if err != nil {
		return fmt.Errorf("re-stat current executable: %w", err)
	}
	if !sameExeIdentity(initialInfo, info) {
		return fmt.Errorf("executable changed during confirmation (another update may have run); aborting to avoid downgrade. Re-run `yolorouter update`")
	}

	staging, err := writeStagedBinary(filepath.Dir(exe), binaryBytes, info.Mode())
	if err != nil {
		return fmt.Errorf("write new binary: %w", err)
	}

	// Re-validate immediately before the rename: an external package manager
	// could have replaced the binary during staging/fsync (the update lock
	// only serializes update-vs-update, not external tools). If it changed,
	// abort rather than overwrite the newer installation.
	preRename, err := os.Stat(exe)
	if err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("re-stat current executable before replace: %w", err)
	}
	// The sameExeIdentity check against initialInfo runs inside replaceBinary
	// immediately before the rename — re-checking it here would be redundant.
	// preRename is statted only to capture ownership/mode for the chown below.
	// Preserve the original uid/gid (e.g. root:yolorouter) — writeStagedBinary
	// created the staging as root:root under sudo, and a service account in
	// the original group couldn't execute the replaced binary. chown clears
	// setuid/setgid on Unix, so reapply the mode afterward.
	if uid, gid := ownershipOf(preRename); uid >= 0 || gid >= 0 {
		if err := os.Chown(staging, uid, gid); err != nil {
			_ = os.Remove(staging)
			return fmt.Errorf("chown staging to match executable: %w", err)
		}
		if err := os.Chmod(staging, preRename.Mode()); err != nil {
			_ = os.Remove(staging)
			return fmt.Errorf("reapply staging mode after chown: %w", err)
		}
		// chown/chmod dirtied inode metadata AFTER writeStagedBinary's
		// Sync+Close, so fsync the staging file again — otherwise a crash
		// before the rename could lose the ownership/mode change.
		if f, syncErr := os.Open(staging); syncErr == nil {
			_ = f.Sync()
			_ = f.Close()
		}
	}

	backupPath, err := replaceBinary(exe, staging, initialInfo)
	if err != nil {
		_ = os.Remove(staging)
		return err
	}

	fmt.Printf("upgraded to %s\n", rel.TagName)
	fmt.Printf("binary backup: %s\n", backupPath)
	fmt.Printf("  rollback BEFORE restart: stop the service, then: mv %s %s\n", shellQuote(backupPath), shellQuote(exe))
	if runtime.GOOS == "linux" {
		// Atomic rename creates a fresh inode; Linux file capabilities
		// (security.capability xattr, e.g. cap_net_bind_service via setcap)
		// do not carry over, so a non-root service binding a privileged port
		// would fail to restart. Tell the operator to reapply them.
		fmt.Println("note: if this binary uses Linux file capabilities (e.g. cap_net_bind_service),")
		fmt.Println("      reapply them on the new binary — atomic rename does not preserve xattrs.")
	}
	fmt.Println("restart yolorouter to apply (will run database migrations)")
	return nil
}

// githubAsset mirrors the relevant fields of GitHub's release-asset JSON.
type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// githubRelease mirrors the relevant fields of GitHub's release JSON. The
// `update` command needs assets (to find the per-platform archive +
// checksums.txt), unlike VersionService which only reads tag_name/html_url.
type githubRelease struct {
	TagName string        `json:"tag_name"`
	HTMLURL string        `json:"html_url"`
	Assets  []githubAsset `json:"assets"`
}

func fetchLatestReleaseWith(ctx context.Context, client *http.Client, repo string) (*githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "yolorouter")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github returned %d for %s", resp.StatusCode, url)
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release JSON: %w", err)
	}
	return &rel, nil
}

func downloadWith(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// currentUpdatable rejects a current version the updater cannot safely
// compare against the latest tag: a non-semver "dev" build, and — the case
// that bit git-describe release builds — a prerelease. git-describe strings
// ("v1.2.3-dirty", "v1.2.3-4-gabc") and release candidates ("v1.2.3-rc1")
// are semver prereleases ranked BELOW their release, so an unguarded
// Compare(latest, current) would report the tag as newer and let the updater
// replace a newer dirty build with the older tag. CE ships only exact-tag
// release binaries, so only an exact-tag current is eligible to update.
func currentUpdatable(current string) error {
	if !semver.IsValid(current) {
		return fmt.Errorf("cannot update a non-release build (current version %q); install a release build first", current)
	}
	if semver.Prerelease(current) != "" {
		return fmt.Errorf("cannot update a pre-release or git-describe build (current version %q); checkout the exact release tag and rebuild", current)
	}
	return nil
}

// sameExeIdentity reports whether two FileInfo describe the same on-disk
// executable state. mtime + size is a pragmatic proxy for "did another
// `update` replace this binary": a rename installs a fresh inode whose mtime
// differs from the one recorded at release-selection time. It isn't a perfect
// identity (same-second + same-size collisions), but it's enough to catch the
// common overlap without a cross-platform file lock.
func sameExeIdentity(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// ownershipOf extracts the uid/gid of the file described by info, or returns
// (-1, -1) when not available (non-unix, or Sys() doesn't expose them). -1
// means "don't change" to os.Chown. Reflection avoids a platform-specific
// syscall.Stat_t type assertion that wouldn't compile on windows (where
// update is a no-op anyway).
func ownershipOf(info os.FileInfo) (uid, gid int) {
	if info == nil {
		return -1, -1
	}
	sys := info.Sys()
	if sys == nil {
		return -1, -1
	}
	v := reflect.ValueOf(sys)
	// info.Sys() returns a *syscall.Stat_t (a pointer); FieldByName needs the
	// struct value, so deref first. Bail to "don't change" for anything that
	// isn't a struct (non-unix, or unexpected underlying type).
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return -1, -1
	}
	uidV := v.FieldByName("Uid")
	gidV := v.FieldByName("Gid")
	if !uidV.IsValid() || !gidV.IsValid() {
		return -1, -1
	}
	// Uid/Gid are uint32 on linux/darwin, but other platforms may use int —
	// pick the right accessor by Kind rather than assuming Int()/Uint().
	toInt := func(field reflect.Value) int {
		switch field.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return int(field.Int())
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return int(field.Uint())
		default:
			return -1
		}
	}
	uid, gid = toInt(uidV), toInt(gidV)
	if uid < 0 || gid < 0 {
		return -1, -1
	}
	return uid, gid
}

// acquireUpdateLock takes an inter-process exclusive lock for the update
// operation, so two overlapping `update` invocations can't both pass
// revalidation and then race to replaceBinary (the loser would rename its
// stale downloaded bytes over the newer binary the winner just installed,
// silently downgrading). The lock is a same-directory file created with
// O_CREATE|O_EXCL (atomic on POSIX); held through backup+rename, released on
// return. A crash leaves the lock file behind — the error message tells the
// operator to remove it.
func acquireUpdateLock(dir string) (string, *os.File, error) {
	lockPath := filepath.Join(dir, ".yolorouter-update.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return lockPath, nil, fmt.Errorf("another update appears to be in progress (lock %s); aborting. If no update is running, remove that file and retry", lockPath)
		}
		// EACCES (dir not writable), EROFS (read-only fs), etc. — NOT a lock
		// collision. Wrap distinctly so the operator fixes the real cause
		// rather than being told to remove a lock that was never created.
		return lockPath, nil, fmt.Errorf("create update lock %s: %w (check directory write permissions / filesystem)", lockPath, err)
	}
	_, _ = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	return lockPath, f, nil
}

// shellQuote wraps s in single quotes so the shell treats it literally.
// Embedded single quotes are escaped by ending the single-quoted string,
// emitting a backslash-escaped single quote, and starting a new single-quoted
// string (the standard shell idiom; written out in prose because gofmt rewrites
// the literal escape sequence inside a comment). Go's %q produces a
// double-quoted string where $, backticks, and dollar-parens still expand or
// execute when an operator pastes a printed command — unsafe for paths the
// user will paste (config path, rollback mv operands).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// backupCommand returns the db:backup hint string for the rollback notice.
// With no --config, the bare command loads the default config (the same one
// update used); appending `--config ` with an empty value would fail the flag
// parse, so the flag is omitted for the default path and shell-quoted
// otherwise.
func backupCommand(configPath string) string {
	if configPath == "" {
		return "  yolorouter db:backup"
	}
	return "  yolorouter db:backup --config " + shellQuote(configPath)
}

// matchAsset finds the archive for the current platform in the release's
// assets, named per .goreleaser.yaml's archive name_template:
// yolorouter_v{ver}_{goos}_{goarch}.tar.gz (the leading v matches the
// GitHub tag_name, since the update command resolves ver from tag_name).
func matchAsset(assets []githubAsset, goos, goarch, ver string) (githubAsset, error) {
	want := fmt.Sprintf("yolorouter_%s_%s_%s.tar.gz", ver, goos, goarch)
	return findAsset(assets, want)
}

func findAsset(assets []githubAsset, name string) (githubAsset, error) {
	for _, a := range assets {
		if a.Name == name {
			return a, nil
		}
	}
	return githubAsset{}, fmt.Errorf("no asset named %q in release; available: %s", name, assetNames(assets))
}

func assetNames(assets []githubAsset) string {
	names := make([]string, len(assets))
	for i, a := range assets {
		names[i] = a.Name
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// verifyChecksum recomputes the SHA256 of the downloaded archive and compares
// it against the entry goreleaser's checksums.txt carries for that asset. The
// checksums.txt format is one "<sha256>  <name>" line per asset.
func verifyChecksum(assetBytes []byte, assetName string, checksumsTxt []byte) error {
	sum := sha256.Sum256(assetBytes)
	got := hex.EncodeToString(sum[:])
	want, ok := parseChecksums(checksumsTxt)[assetName]
	if !ok {
		return fmt.Errorf("checksums.txt has no entry for %q", assetName)
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 mismatch for %q: downloaded %s, expected %s", assetName, got, want)
	}
	return nil
}

func parseChecksums(txt []byte) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(string(txt), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			m[fields[1]] = fields[0]
		}
	}
	return m
}

// extractBinary unpacks the tar.gz archive and returns the bytes of the
// executable (basename binaryName), wherever the archive placed it.
func extractBinary(assetBytes []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(assetBytes))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == binaryName {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read %s from tar: %w", hdr.Name, err)
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("binary %q not found in archive", binaryName)
}

// isExecutable sniffs the leading magic bytes for the executable formats
// CE release builds ship (ELF for linux, Mach-O for darwin). A plain
// non-zero check isn't enough: a corrupt extraction that produced garbage
// bytes would otherwise be chmod'd executable and silently installed.
func isExecutable(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	// ELF: 0x7f 'E' 'L' 'F'
	if data[0] == 0x7f && data[1] == 'E' && data[2] == 'L' && data[3] == 'F' {
		return true
	}
	// Mach-O magic numbers (32/64-bit, little/big-endian variants).
	magic := [4]byte{data[0], data[1], data[2], data[3]}
	for _, m := range [][4]byte{
		{0xfe, 0xed, 0xfa, 0xce}, // MH_MAGIC (BE, 32-bit)
		{0xfe, 0xed, 0xfa, 0xcf}, // MH_MAGIC_64 (BE, 64-bit)
		{0xce, 0xfa, 0xed, 0xfe}, // MH_CIGAM (LE, 32-bit)
		{0xcf, 0xfa, 0xed, 0xfe}, // MH_CIGAM_64 (LE, 64-bit)
	} {
		if magic == m {
			return true
		}
	}
	return false
}

// writeStagedBinary writes data to a uniquely-named, exclusively-created
// temporary file in dir, fsyncs, and closes — so the bytes are durable on
// disk before the atomic rename. It returns the temp path for the caller to
// rename over the live binary.
//
// os.CreateTemp (O_CREATE|O_EXCL, randomized name) is used rather than a
// fixed "<exe>.new" path: two overlapping updaters would otherwise truncate
// the same inode, and a predictable path in a shared/sticky directory can be
// symlink-clobbered to overwrite an arbitrary privileged file. The file is
// chmod'd to mode AFTER creation — CreateTemp creates with 0600 and a
// restrictive umask (e.g. 077) would otherwise strip the executable bit from
// an os.OpenFile mode arg, leaving the replaced binary unrunnable under a
// service account. On any error the file is removed so a
// half-written staging file can never be mistaken for a valid upgrade.
func writeStagedBinary(dir string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, "yolorouter.new.*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	cleanup := func() { _ = f.Close(); _ = os.Remove(path) }
	if _, err := f.Write(data); err != nil {
		cleanup()
		return "", err
	}
	// Apply the executable mode BEFORE the final Sync so the persisted file
	// already carries it — CreateTemp creates with 0600, and a chmod after
	// Sync dirties inode metadata without a follow-up fsync, so a crash could
	// leave the installed binary non-executable. f.Chmod on the open handle
	// also avoids umask masking an os.OpenFile mode arg.
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return "", err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// replaceBinary backs up the current binary to currentPath+".bak" and then
// atomically renames the staging path over it. On linux/mac the rename
// overwrites the running binary's path while the running process keeps the
// old inode until restart. The parent directory is fsync'd
// so the rename itself survives a crash.
func replaceBinary(currentPath, stagingPath string, initialInfo os.FileInfo) (string, error) {
	backupPath := currentPath + ".bak"
	if err := copyFile(currentPath, backupPath); err != nil {
		return "", fmt.Errorf("backup current binary: %w", err)
	}
	// Re-validate immediately before the rename: an external installer could
	// have replaced the binary during the backup copy (the update lock only
	// serializes update-vs-update). If it changed since initialInfo, abort
	// rather than overwrite the newer installation.
	preRename, err := os.Stat(currentPath)
	if err != nil {
		return "", fmt.Errorf("re-stat current executable before rename: %w", err)
	}
	if !sameExeIdentity(initialInfo, preRename) {
		return "", fmt.Errorf("executable changed during backup (external tool may have updated it); aborting to avoid downgrade. Re-run `yolorouter update`")
	}
	if err := os.Rename(stagingPath, currentPath); err != nil {
		return "", fmt.Errorf("replace binary: %w", err)
	}
	// Parent-dir fsync is best-effort: the rename is already committed, so a
	// failure only weakens crash-durability, not the upgrade itself. Don't
	// return an error — that would make runUpdate exit non-zero after an
	// irreversible mutation, leaving the operator with neither success nor
	// rollback guidance.
	_ = fsyncDir(filepath.Dir(currentPath))
	return backupPath, nil
}

// copyFile copies src to dst preserving src's mode, via a uniquely-named,
// exclusively-created temp file in dst's directory + rename. The temp is
// chmod'd to src's mode after creation (umask-safe). This mirrors
// writeStagedBinary's hardening: an interrupted copy can't leave a truncated
// .bak a user might mistake for a rollback, two overlapping copies can't
// truncate each other, and a predictable .bak.tmp can't be symlink-clobbered
// in a sticky dir.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp.*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	cleanup := func() { _ = out.Close(); _ = os.Remove(tmp) }
	if _, err := io.Copy(out, in); err != nil {
		cleanup()
		return err
	}
	// Preserve src's uid/gid (e.g. root:yolorouter) — CreateTemp created the
	// .bak as root:root under sudo, and an advertised rollback would be
	// unrunnable under the original service account. chown BEFORE chmod:
	// chown clears setuid/setgid on Unix, so apply ownership first then let
	// the chmod re-establish any special bits.
	if uid, gid := ownershipOf(info); uid >= 0 || gid >= 0 {
		if err := os.Chown(tmp, uid, gid); err != nil {
			cleanup()
			return err
		}
	}
	// chmod BEFORE Sync so the persisted temp already carries src's mode — a
	// chmod after Sync dirties metadata without a follow-up fsync.
	if err := out.Chmod(info.Mode()); err != nil {
		cleanup()
		return err
	}
	if err := out.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Remove the fully-written temp so repeated update attempts don't
		// leak another binary-sized file beside the executable.
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
