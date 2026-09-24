//go:build !windows

package database

import (
	"errors"
	"path/filepath"
	"syscall"
)

// OSFreeSpaceProbe is the production FreeSpaceProbe: statfs(2) on the
// directory, reporting Bavail (blocks available to unprivileged writers —
// the budget a backup actually draws from, not the larger Bfree that
// includes root's reserve) times the filesystem block size.
var OSFreeSpaceProbe FreeSpaceProbe = osFreeSpace

// osFreeSpace walks up from dir to the closest existing ancestor before
// statfs'ing: the backup directory does not exist yet before the first
// upgrade, and its yet-to-be-created parent chain lives on the same
// filesystem by construction (the backup directory is created inside the
// database file's directory). ENOTDIR gets the same treatment — a path
// component replaced by a regular file is just another "not there (yet)"
// shape. Anything else (permissions, I/O error) is returned as-is and the
// precheck layer decides what to do with it.
func osFreeSpace(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			if parent := filepath.Dir(dir); parent != dir {
				return osFreeSpace(parent)
			}
		}
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
