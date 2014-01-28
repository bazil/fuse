package fstestutil

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"errors"
	"io/ioutil"
	"log"
	"os"
	"testing"
)

// Mount contains information about the mount for the test to use.
type Mount struct {
	// Dir is the temporary directory where the filesystem is mounted.
	Dir string

	Conn *fuse.Conn

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
	err := fuse.Unmount(mnt.Dir)
	if err != nil {
		// TODO do more than log?
		log.Printf("unmount error: %v", err)
	}
	<-mnt.done
	os.Remove(mnt.Dir)
}

// Mounted mounts the fuse.Server at a temporary directory.
//
// After successful return, caller must clean up by calling Close.
func Mounted(srv *fs.Server) (*Mount, error) {
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
		Conn:  c,
		Error: serveErr,
		done:  done,
	}
	go func() {
		defer close(done)
		serveErr <- srv.Serve(c)
	}()
	return mnt, nil
}

// MountedT mounts the filesystem at a temporary directory,
// directing it's debug log to the testing logger.
//
// It also waits until the filesystem is known to be visible (OS X
// workaround).
//
// See Mounted for usage.
//
// The debug log is not enabled by default. Use `-fuse.debug` or call
// DebugByDefault to enable.
func MountedT(t testing.TB, filesys fs.FS) (*Mount, error) {
	srv := &fs.Server{
		FS: filesys,
	}
	if debug {
		srv.Debug = func(msg interface{}) {
			t.Logf("FUSE: %s", msg)
		}
	}
	mnt, err := Mounted(srv)
	if err != nil {
		return mnt, err
	}

	select {
	case <-mnt.Conn.Ready:
		if mnt.Conn.MountError != nil {
			return nil, err
		}
		return mnt, err
	case err = <-mnt.Error:
		// Serve quit early
		if err != nil {
			return nil, err
		}
		return nil, errors.New("Serve exited early")
	}
}
