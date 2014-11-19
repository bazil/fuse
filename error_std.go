// +build !darwin
// +build !freebsd

package fuse

import "syscall"

const ENODATA = Errno(syscall.ENODATA)

func translateGetxattrError(err Error) Error {
	return err
}
