// Package syscallx used to provide wrappers for syscalls.
//
// It is no longer needed.
//
// Deprecated: Use golang.org/x/sys/unix directly.
package syscallx

import "golang.org/x/sys/unix"

// Deprecated: Use golang.org/x/sys/unic directly.
func Getxattr(path string, attr string, dest []byte) (sz int, err error) {
	return unix.Getxattr(path, attr, dest)
}

// Deprecated: Use golang.org/x/sys/unic directly.
func Listxattr(path string, dest []byte) (sz int, err error) {
	return unix.Listxattr(path, dest)
}

// Deprecated: Use golang.org/x/sys/unic directly.
func Setxattr(path string, attr string, data []byte, flags int) (err error) {
	return unix.Setxattr(path, attr, data, flags)
}

// Deprecated: Use golang.org/x/sys/unic directly.
func Removexattr(path string, attr string) (err error) {
	return unix.Removexattr(path, attr)
}

// Deprecated: Use golang.org/x/sys/unix directly.
func Msync(b []byte, flags int) (err error) {
	return unix.Msync(b, flags)
}
