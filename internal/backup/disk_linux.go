//go:build linux

package backup

import "syscall"

// diskFreeBytes returns the free bytes available to the panel on the
// filesystem containing path. Availability is measured for unprivileged
// writers (Bavail), which is the number that matters for a service that runs
// as root but writes into a normal data directory.
func diskFreeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
