//go:build linux

package store

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

const (
	configServiceUser  = "ts3panel"
	configServiceGroup = "ts3panel"
)

// preserveOwner keeps the existing owner when an atomic rename replaces a
// state/config file. This matters for `reset-password`: root writes the token,
// but the panel process (e.g. the ts3panel user) must still be able to read and
// replace state.json afterwards.
func preserveOwner(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := os.Chown(path, int(st.Uid), int(st.Gid)); err != nil {
		// An unprivileged process may be unable to chown even to its own uid on
		// some filesystems. Since the file mode is 0600, group ownership is not
		// security-relevant; only fail when the owner would actually change.
		if os.Geteuid() != 0 && errors.Is(err, syscall.EPERM) && int(st.Uid) == os.Geteuid() {
			return nil
		}
		return fmt.Errorf("chown to %d:%d: %w", st.Uid, st.Gid, err)
	}
	return nil
}

// enforceConfigOwner keeps legacy non-root deployments working: when the panel
// runs as the ts3panel account it must own config.toml to rewrite providers.
// The supported deployment (errata E4) runs the service as root and keeps
// config.toml root:root 0600, so root never transfers ownership away.
func enforceConfigOwner(path string) error {
	if os.Geteuid() == 0 {
		return nil
	}
	account, err := user.Lookup(configServiceUser)
	if err != nil {
		return nil
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return fmt.Errorf("invalid uid for %s: %w", configServiceUser, err)
	}
	if os.Geteuid() != 0 && os.Geteuid() != uid {
		return nil
	}

	group, err := user.LookupGroup(configServiceGroup)
	if err != nil {
		return fmt.Errorf("lookup group %s: %w", configServiceGroup, err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return fmt.Errorf("invalid gid for %s: %w", configServiceGroup, err)
	}

	if err := os.Chown(path, uid, gid); err != nil {
		if fi, statErr := os.Stat(path); statErr == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid && int(st.Gid) == gid {
				return nil
			}
		}
		return fmt.Errorf("chown config.toml to %s:%s: %w", configServiceUser, configServiceGroup, err)
	}
	return nil
}
