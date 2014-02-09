// +build !darwin,linux

package fuse

func translateGetxattrError(err Error) Error {
	return err
}
