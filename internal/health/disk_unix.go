//go:build !windows

package health

import "syscall"

// freeSpace reports the bytes available to an unprivileged writer at path.
func freeSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
