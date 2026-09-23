//go:build !darwin && !linux

package httpapi

// diskFreeBytes is not implemented here; the scratch free-space check is skipped.
func diskFreeBytes(string) (uint64, error) { return 0, errDiskFreeUnsupported }
