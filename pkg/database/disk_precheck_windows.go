//go:build windows

package database

// OSFreeSpaceProbe is the production FreeSpaceProbe on windows: probing is
// deliberately unimplemented. The SQLite backup tooling treats windows as
// out of scope (drive-letter and backslash paths are not handled), so rather
// than grow a second platform implementation, the precheck reports
// "unsupported" and callers let the migration proceed under the pre-existing
// fail-closed backup semantics.
var OSFreeSpaceProbe FreeSpaceProbe = func(dir string) (int64, error) {
	return 0, ErrFreeSpaceProbeUnsupported
}
