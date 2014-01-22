package fs_test

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
	"syscall"
	"testing"
	"time"
)

func init() {
	fstestutil.DebugByDefault()
}

type badRootFS struct{}

func (badRootFS) Root() (fs.Node, fuse.Error) {
	// pick a really distinct error, to identify it later
	return nil, fuse.Errno(syscall.ENAMETOOLONG)
}

func TestRootErr(t *testing.T) {
	mnt, err := fstestutil.MountedT(t, badRootFS{})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	select {
	case err = <-mnt.Error:
		if err == nil {
			t.Errorf("expected an error")
		}
		// TODO this should not be a textual comparison, Serve hides
		// details
		if err.Error() != "cannot obtain root node: file name too long" {
			t.Errorf("Unexpected error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Serve did not return an error as expected, aborting")
	}
}
