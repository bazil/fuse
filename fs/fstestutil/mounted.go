package fstestutil

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"io/ioutil"
	"os"
)

// Mount contains information about the mount for the test to use.
type Mount struct {
	// Dir is the temporary directory where the filesystem is mounted.
	Dir string

	// Error will receive the return value of Serve.
	Error <-chan error

	done   <-chan struct{}
	closed bool
}

// Close unmounts the filesystem and waits for fs.Serve to return. Any
// returned error will be stored in Err. It is safe to call Close
// multiple times.
func (mnt *Mount) Close() {
	if mnt.closed {
		return
	}
	mnt.closed = true
	_ = fuse.Unmount(mnt.Dir)
	<-mnt.done
	os.Remove(mnt.Dir)
}

// Mounted mounts the filesystem at a temporary directory.
//
// After successful return, caller must clean up by calling Close.
func Mounted(filesys fs.FS) (*Mount, error) {
	dir, err := ioutil.TempDir("", "fusetest")
	if err != nil {
		return nil, err
	}
	c, err := fuse.Mount(dir)
	if err != nil {
		return nil, err
	}

	done := make(chan struct{})
	serveErr := make(chan error, 1)
	mnt := &Mount{
		Dir:   dir,
		Error: serveErr,
		done:  done,
	}
	go func() {
		defer close(done)
		serveErr <- fs.Serve(c, filesys)
	}()
	return mnt, nil
}
