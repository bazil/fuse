package fuse

import (
	"os"
	"strings"
)

func dummyOption(conf *MountConfig) error {
	return nil
}

// MountConfig holds the configuration for a mount operation.
// Use it by passing MountOption values to Mount.
type MountConfig struct {
	options             map[string]string
	MaxReadahead        uint32
	initFlags           InitFlags
	maxBackground       uint16
	congestionThreshold uint16

	mountFunc  MFunc
	umountFunc UMFunc
}

func escapeComma(s string) string {
	s = strings.Replace(s, `\`, `\\`, -1)
	s = strings.Replace(s, `,`, `\,`, -1)
	return s
}

// GetOptions makes a string of options suitable for passing to FUSE
// mount flag `-o`. Returns an empty string if no options were set.
// Any platform specific adjustments should happen before the call.
func (m *MountConfig) GetOptions() string {
	var opts []string
	for k, v := range m.options {
		k = escapeComma(k)
		if v != "" {
			k += "=" + escapeComma(v)
		}
		opts = append(opts, k)
	}
	return strings.Join(opts, ",")
}

type mountOption func(*MountConfig) error

// MountOption is passed to Mount to change the behavior of the mount.
type MountOption mountOption

// MFunc is a user-specified mount function.
type MFunc func(string, *MountConfig) (*os.File, error)

// UMFunc is a user-specified umount function.
type UMFunc func(string) error

// FSName sets the file system name (also called source) that is
// visible in the list of mounted file systems.
//
// FreeBSD ignores this option.
func FSName(name string) MountOption {
	return func(conf *MountConfig) error {
		conf.options["fsname"] = name
		return nil
	}
}

// Subtype sets the subtype of the mount. The main type is always
// `fuse`. The type in a list of mounted file systems will look like
// `fuse.foo`.
//
// FreeBSD ignores this option.
func Subtype(fstype string) MountOption {
	return func(conf *MountConfig) error {
		conf.options["subtype"] = fstype
		return nil
	}
}

// DaemonTimeout sets the time in seconds between a request and a reply before
// the FUSE mount is declared dead.
//
// FreeBSD only. Others ignore this option.
func DaemonTimeout(name string) MountOption {
	return daemonTimeout(name)
}

// AllowOther allows other users to access the file system.
func AllowOther() MountOption {
	return func(conf *MountConfig) error {
		conf.options["allow_other"] = ""
		return nil
	}
}

// AllowDev enables interpreting character or block special devices on the
// filesystem.
func AllowDev() MountOption {
	return func(conf *MountConfig) error {
		conf.options["dev"] = ""
		return nil
	}
}

// AllowSUID allows set-user-identifier or set-group-identifier bits to take
// effect.
func AllowSUID() MountOption {
	return func(conf *MountConfig) error {
		conf.options["suid"] = ""
		return nil
	}
}

// DefaultPermissions makes the kernel enforce access control based on
// the file mode (as in chmod).
//
// Without this option, the Node itself decides what is and is not
// allowed. This is normally ok because FUSE file systems cannot be
// accessed by other users without AllowOther.
//
// FreeBSD ignores this option.
func DefaultPermissions() MountOption {
	return func(conf *MountConfig) error {
		conf.options["default_permissions"] = ""
		return nil
	}
}

// ReadOnly makes the mount read-only.
func ReadOnly() MountOption {
	return func(conf *MountConfig) error {
		conf.options["ro"] = ""
		return nil
	}
}

// MaxReadahead sets the number of bytes that can be prefetched for
// sequential reads. The kernel can enforce a maximum value lower than
// this.
//
// This setting makes the kernel perform speculative reads that do not
// originate from any client process. This usually tremendously
// improves read performance.
func MaxReadahead(n uint32) MountOption {
	return func(conf *MountConfig) error {
		conf.MaxReadahead = n
		return nil
	}
}

// MountFunc allows the user to specify their own mount function.
func MountFunc(f MFunc) MountOption {
	return func(conf *MountConfig) error {
		conf.mountFunc = f
		return nil
	}
}

// UMountFunc allow the user to specify their own umount function.
func UMountFunc(f UMFunc) MountOption {
	return func(conf *MountConfig) error {
		conf.umountFunc = f
		return nil
	}
}

// AsyncRead enables multiple outstanding read requests for the same
// handle. Without this, there is at most one request in flight at a
// time.
func AsyncRead() MountOption {
	return func(conf *MountConfig) error {
		conf.initFlags |= InitAsyncRead
		return nil
	}
}

// WritebackCache enables the kernel to buffer writes before sending
// them to the FUSE server. Without this, writethrough caching is
// used.
func WritebackCache() MountOption {
	return func(conf *MountConfig) error {
		conf.initFlags |= InitWritebackCache
		return nil
	}
}

// CacheSymlinks enables the kernel to cache symlink contents.
func CacheSymlinks() MountOption {
	return func(conf *MountConfig) error {
		conf.initFlags |= InitCacheSymlinks
		return nil
	}
}

// ExplicitInvalidateData stops the kernel from invalidating cached file data automatically.
// Use e.g. `InvalidateNodeData` to invalidate cached data as needed.
func ExplicitInvalidateData() MountOption {
	return func(conf *MountConfig) error {
		conf.initFlags |= InitExplicitInvalidateData
		return nil
	}
}

// AllowNonEmptyMount allows the mounting over a non-empty directory.
//
// The files in it will be shadowed by the freshly created mount. By
// default these mounts are rejected to prevent accidental covering up
// of data, which could for example prevent automatic backup.
func AllowNonEmptyMount() MountOption {
	return func(conf *MountConfig) error {
		conf.options["nonempty"] = ""
		return nil
	}
}

// MaxBackground sets the maximum number of FUSE requests the kernel
// will submit in the background. Background requests are used when an
// immediate answer is not needed. This may help with request latency.
//
// On Linux, this can be adjusted on the fly with
// /sys/fs/fuse/connections/CONN/max_background
func MaxBackground(n uint16) MountOption {
	return func(conf *MountConfig) error {
		conf.maxBackground = n
		return nil
	}
}

// CongestionThreshold sets the number of outstanding background FUSE
// requests beyond which the kernel considers the filesystem
// congested. This may help with request latency.
//
// On Linux, this can be adjusted on the fly with
// /sys/fs/fuse/connections/CONN/congestion_threshold
func CongestionThreshold(n uint16) MountOption {
	// TODO to test this, we'd have to figure out our connection id
	// and read /sys
	return func(conf *MountConfig) error {
		conf.congestionThreshold = n
		return nil
	}
}

// LockingFlock enables flock-based (BSD) locking. This is mostly
// useful for distributed filesystems with global locking. Without
// this, kernel manages local locking automatically.
func LockingFlock() MountOption {
	return func(conf *MountConfig) error {
		conf.initFlags |= InitFlockLocks
		return nil
	}
}

// LockingPOSIX enables flock-based (BSD) locking. This is mostly
// useful for distributed filesystems with global locking. Without
// this, kernel manages local locking automatically.
//
// Beware POSIX locks are a broken API with unintuitive behavior for
// callers.
func LockingPOSIX() MountOption {
	return func(conf *MountConfig) error {
		conf.initFlags |= InitPOSIXLocks
		return nil
	}
}

// HandleKillPriv enables SUID/SGID/cap management in the FUSE server.
//
// This is the newer `InitHandleKillPrivV2` interface from FUSE protocol 7.33, not the deprecated one from 7.26.
func HandleKillPriv() MountOption {
	return func(conf *MountConfig) error {
		conf.initFlags |= InitHandleKillPrivV2
		return nil
	}
}
