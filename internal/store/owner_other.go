//go:build !linux

package store

import "os"

// preserveOwner is a no-op outside Linux. ts3panel only supports Linux hosts;
// this file keeps Windows developer builds and cross-platform `go vet` working.
func preserveOwner(string, os.FileInfo) error {
	return nil
}

// enforceConfigOwner is Linux-only; Windows does not have POSIX uid/gid
// ownership and is not a supported panel host.
func enforceConfigOwner(string) error {
	return nil
}
