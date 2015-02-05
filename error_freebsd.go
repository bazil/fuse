package fuse

import "syscall"

const ENODATA = Errno(syscall.ENOATTR)

func translateGetxattrError(err Error) Error {
	return err
}
