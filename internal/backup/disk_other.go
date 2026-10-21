//go:build !linux

package backup

// diskFreeBytes is unavailable on non-Linux hosts. Returning -1 (unknown)
// makes the backup manager skip the pre-flight space check instead of guessing;
// Windows is only a development host for this project.
func diskFreeBytes(string) (int64, error) { return -1, nil }
