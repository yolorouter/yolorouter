package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestMatchAssetFindsPlatformArchive(t *testing.T) {
	assets := []githubAsset{
		{Name: "yolorouter-ce_v0.2.0_linux_amd64.tar.gz"},
		{Name: "yolorouter-ce_v0.2.0_linux_arm64.tar.gz"},
		{Name: "yolorouter-ce_v0.2.0_darwin_amd64.tar.gz"},
		{Name: "checksums.txt"},
	}
	got, err := matchAsset(assets, "linux", "amd64", "v0.2.0")
	if err != nil {
		t.Fatalf("matchAsset: %v", err)
	}
	if got.Name != "yolorouter-ce_v0.2.0_linux_amd64.tar.gz" {
		t.Fatalf("matched %q, want linux_amd64 archive", got.Name)
	}
}

func TestMatchAssetErrorsOnMissingPlatform(t *testing.T) {
	assets := []githubAsset{{Name: "yolorouter-ce_v0.2.0_linux_amd64.tar.gz"}}
	if _, err := matchAsset(assets, "darwin", "arm64", "v0.2.0"); err == nil {
		t.Fatalf("expected error when no asset matches the current platform")
	}
}

func TestFindAssetErrorsListAvailable(t *testing.T) {
	assets := []githubAsset{{Name: "a.tar.gz"}, {Name: "b.tar.gz"}}
	_, err := findAsset(assets, "missing.tar.gz")
	if err == nil {
		t.Fatalf("expected error for missing asset")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("a.tar.gz")) {
		t.Fatalf("error should list available assets, got: %v", err)
	}
}

func TestParseChecksums(t *testing.T) {
	// goreleaser emits one "<sha256>  <name>" line per asset (two spaces).
	txt := []byte("abc123  yolorouter-ce_v0.2.0_linux_amd64.tar.gz\ndef456  checksums.txt\n")
	m := parseChecksums(txt)
	if m["yolorouter-ce_v0.2.0_linux_amd64.tar.gz"] != "abc123" {
		t.Fatalf("archive hash = %q, want abc123", m["yolorouter-ce_v0.2.0_linux_amd64.tar.gz"])
	}
	if m["checksums.txt"] != "def456" {
		t.Fatalf("checksums hash = %q, want def456", m["checksums.txt"])
	}
}

func TestVerifyChecksumAcceptsMatchingHash(t *testing.T) {
	data := []byte("the real archive bytes")
	sum := sha256.Sum256(data)
	name := "yolorouter-ce_v0.2.0_linux_amd64.tar.gz"
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	if err := verifyChecksum(data, name, checksums); err != nil {
		t.Fatalf("matching hash should verify, got: %v", err)
	}
}

func TestVerifyChecksumRejectsTamperedBytes(t *testing.T) {
	name := "asset.tar.gz"
	checksums := []byte("0000000000000000000000000000000000000000000000000000000000000000  " + name + "\n")
	if err := verifyChecksum([]byte("tampered"), name, checksums); err == nil {
		t.Fatalf("a mismatched SHA256 must be rejected")
	}
}

func TestVerifyChecksumRejectsMissingEntry(t *testing.T) {
	checksums := []byte("abc123  other-asset.tar.gz\n")
	if err := verifyChecksum([]byte("x"), "missing.tar.gz", checksums); err == nil {
		t.Fatalf("an asset with no checksums.txt entry must be rejected")
	}
}

func makeArchive(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write tar header for %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar body for %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestExtractBinaryFindsTopLevelBinary(t *testing.T) {
	binary := []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01, 0x02}
	archive := makeArchive(t, map[string][]byte{"yolorouter-ce": binary})
	got, err := extractBinary(archive)
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("extracted bytes mismatch")
	}
}

func TestExtractBinaryFindsNestedBinary(t *testing.T) {
	// goreleaser may wrap the binary in a version-named directory.
	binary := []byte{0x7f, 'E', 'L', 'F'}
	archive := makeArchive(t, map[string][]byte{
		"yolorouter-ce-v0.2.0/yolorouter-ce": binary,
		"README.md":                          []byte("ignore me"),
	})
	got, err := extractBinary(archive)
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("extracted bytes mismatch for nested binary")
	}
}

func TestExtractBinaryErrorsWhenAbsent(t *testing.T) {
	archive := makeArchive(t, map[string][]byte{"README.md": []byte("no binary here")})
	if _, err := extractBinary(archive); err == nil {
		t.Fatalf("expected error when archive has no yolorouter-ce binary")
	}
}

func TestIsExecutable(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"ELF", []byte{0x7f, 'E', 'L', 'F', 0, 0}, true},
		{"MachO LE 64", []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0}, true},
		{"MachO LE 32", []byte{0xce, 0xfa, 0xed, 0xfe}, true},
		{"MachO BE 32", []byte{0xfe, 0xed, 0xfa, 0xce}, true},
		{"MachO BE 64", []byte{0xfe, 0xed, 0xfa, 0xcf}, true},
		{"text garbage", []byte("not an executable at all"), false},
		{"too short", []byte{0, 1}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isExecutable(c.data); got != c.want {
				t.Fatalf("isExecutable(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestWriteStagedBinarySetsExecutableMode(t *testing.T) {
	path, err := writeStagedBinary(t.TempDir(), []byte("x"), 0o755)
	if err != nil {
		t.Fatalf("writeStagedBinary: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("staged binary must be executable, got mode %v", info.Mode())
	}
}

// TestWriteStagedBinaryUsesUniqueExclusivePath verifies the P1/P2 hardening:
// each call gets a distinct randomized path in dir (two overlapping updaters
// can't truncate each other's inode), and the file lives in the requested
// directory rather than a fixed "<exe>.new" predictable for symlink clobbering.
func TestWriteStagedBinaryUsesUniqueExclusivePath(t *testing.T) {
	dir := t.TempDir()
	p1, err := writeStagedBinary(dir, []byte("a"), 0o755)
	if err != nil {
		t.Fatalf("first writeStagedBinary: %v", err)
	}
	p2, err := writeStagedBinary(dir, []byte("b"), 0o755)
	if err != nil {
		t.Fatalf("second writeStagedBinary: %v", err)
	}
	if p1 == p2 {
		t.Fatalf("two writeStagedBinary calls must produce distinct paths, got %q twice", p1)
	}
	if filepath.Dir(p1) != dir || filepath.Dir(p2) != dir {
		t.Fatalf("staging files must live in dir %q, got %q and %q", dir, p1, p2)
	}
}

func TestReplaceBinaryBacksUpAndReplaces(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "yolorouter-ce")
	if err := os.WriteFile(current, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	staging, err := writeStagedBinary(dir, []byte("NEW"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	initialInfo, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}

	backup, err := replaceBinary(current, staging, initialInfo)
	if err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	got, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW" {
		t.Fatalf("current = %q, want NEW", got)
	}
	bak, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(bak) != "OLD" {
		t.Fatalf("backup = %q, want OLD", bak)
	}
	if backup != current+".bak" {
		t.Fatalf("backup path = %q, want %s.bak", backup, current)
	}
	// The replaced binary keeps the staging file's executable permission.
	info, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("replaced binary must stay executable, got %v", info.Mode())
	}
	// The staging file is consumed by the rename — no leftover.
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging file should not exist after replaceBinary")
	}
}

// TestReplaceBinaryOverwritesExistingBackup verifies a second upgrade
// overwrites the prior .bak (the backup always reflects the most recent
// pre-upgrade binary, not some earlier one).
func TestReplaceBinaryOverwritesExistingBackup(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "yolorouter-ce")
	if err := os.WriteFile(current, []byte("V1"), 0o755); err != nil {
		t.Fatal(err)
	}
	// First upgrade.
	staging1, _ := writeStagedBinary(dir, []byte("V2"), 0o755)
	initialInfo1, _ := os.Stat(current)
	if _, err := replaceBinary(current, staging1, initialInfo1); err != nil {
		t.Fatal(err)
	}
	// Second upgrade.
	staging2, _ := writeStagedBinary(dir, []byte("V3"), 0o755)
	initialInfo2, _ := os.Stat(current)
	backup2, err := replaceBinary(current, staging2, initialInfo2)
	if err != nil {
		t.Fatal(err)
	}
	bak, _ := os.ReadFile(backup2)
	if string(bak) != "V2" {
		t.Fatalf("second upgrade's backup = %q, want V2 (the version before this upgrade)", bak)
	}
}

func TestCurrentUpdatable(t *testing.T) {
	cases := []struct {
		name    string
		current string
		wantErr bool
	}{
		{"exact tag accepted", "v0.1.0", false},
		{"newer exact tag accepted", "v1.2.3", false},
		{"non-semver dev rejected", "dev", true},
		{"empty rejected", "", true},
		// git-describe strings — the P1 regression: a dirty tree or commits
		// after the tag produce these, and without the guard the updater
		// would downgrade the newer build to the older tag.
		{"git-describe dirty rejected", "v1.2.3-dirty", true},
		{"git-describe post-tag rejected", "v1.2.3-4-gabc", true},
		{"release candidate rejected", "v1.2.3-rc1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := currentUpdatable(c.current)
			if (err != nil) != c.wantErr {
				t.Fatalf("currentUpdatable(%q) err=%v, wantErr=%v", c.current, err, c.wantErr)
			}
		})
	}
}

func TestSameExeIdentity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "exe")
	if err := os.WriteFile(p, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(p)
	b, _ := os.Stat(p)
	if !sameExeIdentity(a, b) {
		t.Fatalf("two stats of the same file must share identity")
	}
	// Simulate a concurrent update replacing the binary: different size and
	// mtime must break the identity, so the in-flight updater aborts rather
	// than downgrade (Codex review P1).
	if err := os.WriteFile(p, []byte("v2-different-length"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, _ := os.Stat(p)
	if sameExeIdentity(a, c) {
		t.Fatalf("a replaced file (different size/mtime) must not match the original identity")
	}
}

func TestBackupCommand(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"empty path emits bare command", "", "  yolorouter-ce db:backup"},
		{"nonempty path is single-quoted", "/etc/ce/prod.yaml", "  yolorouter-ce db:backup --config '/etc/ce/prod.yaml'"},
		{"path with spaces is single-quoted", "/path with spaces/cfg.yaml", "  yolorouter-ce db:backup --config '/path with spaces/cfg.yaml'"},
		// %q (Go double-quote) would let $ expand / backticks execute when an
		// operator pastes the command — single quotes make it literal.
		{"path with $ stays literal", "/etc/$HOME/cfg.yaml", "  yolorouter-ce db:backup --config '/etc/$HOME/cfg.yaml'"},
		{"path with backtick stays literal", "/a/`whoami`/cfg.yaml", "  yolorouter-ce db:backup --config '/a/`whoami`/cfg.yaml'"},
		// Embedded single quote is escaped with the standard '\'' idiom.
		{"path with single quote is escaped", "/it's/cfg.yaml", "  yolorouter-ce db:backup --config '/it'\\''s/cfg.yaml'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := backupCommand(c.path)
			if got != c.want {
				t.Fatalf("backupCommand(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

func TestAcquireUpdateLock(t *testing.T) {
	dir := t.TempDir()
	// First acquire succeeds and holds the lock.
	lockPath, f1, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = f1.Close(); _ = os.Remove(lockPath) }()
	// A second concurrent acquire must fail — the second updater must abort
	// rather than race to replaceBinary, which would let its stale downloaded
	// bytes overwrite the newer binary the first invocation installs (Codex P1).
	if _, _, err := acquireUpdateLock(dir); err == nil {
		t.Fatalf("second acquire must fail while the first holds the lock")
	}
}
