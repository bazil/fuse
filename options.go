package fuse

import "strings"

// MountConfig holds the configuration for a mount operation.
// Use it by passing MountOption values to Mount.
type MountConfig struct {
	options map[string]string
}

func escapeComma(s string) string {
	s = strings.Replace(s, `\`, `\\`, -1)
	s = strings.Replace(s, `,`, `\,`, -1)
	return s
}

// getOptions makes a string of options suitable for passing to FUSE
// mount flag `-o`. Returns an empty string if no options were set.
// Any platform specific adjustments should happen before the call.
func (m *MountConfig) getOptions() string {
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

// MountOption is passed to Mount to change the behavior of the mount.
type MountOption func(*MountConfig) error

// FSName sets the file system name (also called source) that is
// visible in the list of mounted file systems.
func FSName(name string) MountOption {
	return func(conf *MountConfig) error {
		conf.options["fsname"] = name
		return nil
	}
}
