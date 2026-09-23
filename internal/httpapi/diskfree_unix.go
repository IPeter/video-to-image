//go:build darwin || linux

package httpapi

import (
	"fmt"
	"syscall"
)

// diskFreeBytes returns the bytes available to unprivileged users on dir's volume.
func diskFreeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("httpapi: statfs %s: %w", dir, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:unconvert // field types differ per OS
}
