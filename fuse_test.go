package fuse_test

import (
	"io/ioutil"
	"os"
	"runtime"
	"testing"

	"bazil.org/fuse"
)

func getFeatures(t *testing.T, opts ...fuse.MountOption) fuse.InitFlags {
	tmp, err := ioutil.TempDir("", "fusetest")
	if err != nil {
		t.Fatalf("error creating temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			t.Errorf("error cleaning temp dir: %v", err)
		}
	}()

	conn, err := fuse.Mount(tmp, opts...)
	if err != nil {
		t.Fatalf("error mounting: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("error closing FUSE connection: %v", err)
		}
		if err := fuse.Unmount(tmp); err != nil {
			t.Errorf("error unmounting: %v", err)
		}
	}()

	return conn.Features()
}

func TestFeatures(t *testing.T) {
	run := func(name string, want, notwant fuse.InitFlags, opts ...fuse.MountOption) {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "freebsd" {
				if want&fuse.InitFlockLocks != 0 {
					t.Skip("FreeBSD FUSE does not implement Flock locks")
				}
			}
			got := getFeatures(t, opts...)
			t.Logf("features: %v", got)
			missing := want &^ got
			if missing != 0 {
				t.Errorf("missing: %v", missing)
			}
			extra := got & notwant
			if extra != 0 {
				t.Errorf("extra: %v", extra)
			}
		})
	}

	run("bare", 0, fuse.InitPOSIXLocks|fuse.InitFlockLocks)
	run("LockingFlock", fuse.InitFlockLocks, 0, fuse.LockingFlock())
	run("LockingPOSIX", fuse.InitPOSIXLocks, 0, fuse.LockingPOSIX())
	run("AsyncRead", fuse.InitAsyncRead, 0, fuse.AsyncRead())
	run("WritebackCache", fuse.InitWritebackCache, 0, fuse.WritebackCache())
}
