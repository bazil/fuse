package fstestutil

import (
	"errors"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

// Mount contains information about the mount for the test to use.
type Mount struct {
	// Dir is the temporary directory where the filesystem is mounted.
	Dir string

	Conn   *fuse.Conn
	Server *fs.Server

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
	prev := ""
	for tries := 0; tries < 1000; tries++ {
		err := fuse.Unmount(mnt.Dir)
		if err != nil {
			msg := err.Error()
			// hide repeating errors
			if msg != prev {
				// TODO do more than log?

				// silence a very common message we can't do anything
				// about, for the first few tries. it'll still show if
				// the condition persists.
				if !strings.HasSuffix(err.Error(), ": Device or resource busy") || tries > 10 {
					log.Printf("unmount error: %v", err)
					prev = msg
				}
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}
	<-mnt.done
	mnt.Conn.Close()
	os.Remove(mnt.Dir)
}

// MountedFunc mounts a filesystem at a temporary directory. The
// filesystem used is constructed by calling a function, to allow
// storing fuse.Conn and fs.Server in the FS.
//
// It also waits until the filesystem is known to be visible (OS X
// workaround).
//
// After successful return, caller must clean up by calling Close.
func MountedFunc(fn func(*Mount) fs.FS, conf *fs.Config, options ...fuse.MountOption) (*Mount, error) {
	dir, err := ioutil.TempDir("", "fusetest")
	if err != nil {
		return nil, err
	}
	c, err := fuse.Mount(dir, options...)
	if err != nil {
		return nil, err
	}
	server := fs.New(c, conf)
	done := make(chan struct{})
	serveErr := make(chan error, 1)
	mnt := &Mount{
		Dir:    dir,
		Conn:   c,
		Server: server,
		Error:  serveErr,
		done:   done,
	}
	filesys := fn(mnt)
	go func() {
		defer close(done)
		serveErr <- server.Serve(filesys)
	}()

	select {
	case <-mnt.Conn.Ready:
		if err := mnt.Conn.MountError; err != nil {
			return nil, err
		}
		return mnt, nil
	case err = <-mnt.Error:
		// Serve quit early
		if err != nil {
			return nil, err
		}
		//lint:ignore ST1005 uppercase because it's an idenfier
		return nil, errors.New("Serve exited early")
	}
}

// Mounted mounts the fuse.Server at a temporary directory.
//
// It also waits until the filesystem is known to be visible (OS X
// workaround).
//
// After successful return, caller must clean up by calling Close.
func Mounted(filesys fs.FS, conf *fs.Config, options ...fuse.MountOption) (*Mount, error) {
	fn := func(*Mount) fs.FS { return filesys }
	return MountedFunc(fn, conf, options...)
}

// MountedFuncT mounts a filesystem at a temporary directory,
// directing it's debug log to the testing logger.
//
// See MountedFunc for usage.
//
// The debug log is not enabled by default. Use `-fuse.debug` or call
// DebugByDefault to enable.
func MountedFuncT(t testing.TB, fn func(*Mount) fs.FS, conf *fs.Config, options ...fuse.MountOption) (*Mount, error) {
	if conf == nil {
		conf = &fs.Config{}
	}
	if debug && conf.Debug == nil {
		conf.Debug = func(msg interface{}) {
			t.Logf("FUSE: %s", msg)
		}
	}
	return MountedFunc(fn, conf, options...)
}

// MountedT mounts the filesystem at a temporary directory,
// directing it's debug log to the testing logger.
//
// See Mounted for usage.
//
// The debug log is not enabled by default. Use `-fuse.debug` or call
// DebugByDefault to enable.
func MountedT(t testing.TB, filesys fs.FS, conf *fs.Config, options ...fuse.MountOption) (*Mount, error) {
	fn := func(*Mount) fs.FS { return filesys }
	return MountedFuncT(t, fn, conf, options...)
}
