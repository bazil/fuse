package fs_test

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
	"bazil.org/fuse/fuseutil"
	"io/ioutil"
	"os"
	"syscall"
	"testing"
	"time"
)

func init() {
	fstestutil.DebugByDefault()
}

// childMapFS is an FS with one fixed child named "child".
type childMapFS map[string]fs.Node

var _ = fs.FS(childMapFS{})
var _ = fs.Node(childMapFS{})
var _ = fs.NodeStringLookuper(childMapFS{})

func (f childMapFS) Attr() fuse.Attr {
	return fuse.Attr{Inode: 1, Mode: os.ModeDir | 0777}
}

func (f childMapFS) Root() (fs.Node, fuse.Error) {
	return f, nil
}

func (f childMapFS) Lookup(name string, intr fs.Intr) (fs.Node, fuse.Error) {
	child, ok := f[name]
	if !ok {
		return nil, fuse.ENOENT
	}
	return child, nil
}

// file can be embedded in a struct to make it look like a file.
type file struct{}

func (f file) Attr() fuse.Attr { return fuse.Attr{Mode: 0666} }

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

type testStatFS struct{}

func (f testStatFS) Root() (fs.Node, fuse.Error) {
	return f, nil
}

func (f testStatFS) Attr() fuse.Attr {
	return fuse.Attr{Inode: 1, Mode: os.ModeDir | 0777}
}

func (f testStatFS) Statfs(req *fuse.StatfsRequest, resp *fuse.StatfsResponse, int fs.Intr) fuse.Error {
	resp.Blocks = 42
	resp.Files = 13
	return nil
}

func TestStatfs(t *testing.T) {
	mnt, err := fstestutil.MountedT(t, testStatFS{})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	{
		var st syscall.Statfs_t
		err = syscall.Statfs(mnt.Dir, &st)
		if err != nil {
			t.Errorf("Statfs failed: %v", err)
		}
		t.Logf("Statfs got: %#v", st)
		if g, e := st.Blocks, uint64(42); g != e {
			t.Errorf("got Blocks = %q; want %q", g, e)
		}
		if g, e := st.Files, uint64(13); g != e {
			t.Errorf("got Files = %d; want %d", g, e)
		}
	}

	{
		var st syscall.Statfs_t
		f, err := os.Open(mnt.Dir)
		if err != nil {
			t.Errorf("Open for fstatfs failed: %v", err)
		}
		defer f.Close()
		err = syscall.Fstatfs(int(f.Fd()), &st)
		if err != nil {
			t.Errorf("Fstatfs failed: %v", err)
		}
		t.Logf("Fstatfs got: %#v", st)
		if g, e := st.Blocks, uint64(42); g != e {
			t.Errorf("got Blocks = %q; want %q", g, e)
		}
		if g, e := st.Files, uint64(13); g != e {
			t.Errorf("got Files = %d; want %d", g, e)
		}
	}

}

// Test Stat of root.

type root struct{}

func (f root) Root() (fs.Node, fuse.Error) {
	return f, nil
}

func (root) Attr() fuse.Attr {
	return fuse.Attr{Inode: 1, Mode: os.ModeDir | 0555}
}

func TestStatRoot(t *testing.T) {
	mnt, err := fstestutil.MountedT(t, root{})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	fi, err := os.Stat(mnt.Dir)
	if err != nil {
		t.Fatalf("root getattr failed with %v", err)
	}
	mode := fi.Mode()
	if (mode & os.ModeType) != os.ModeDir {
		t.Errorf("root is not a directory: %#v", fi)
	}
	if mode.Perm() != 0555 {
		t.Errorf("root has weird access mode: %v", mode.Perm())
	}
	switch stat := fi.Sys().(type) {
	case *syscall.Stat_t:
		if stat.Ino != 1 {
			t.Errorf("root has wrong inode: %v", stat.Ino)
		}
		if stat.Nlink != 1 {
			t.Errorf("root has wrong link count: %v", stat.Nlink)
		}
		if stat.Uid != 0 {
			t.Errorf("root has wrong uid: %d", stat.Uid)
		}
		if stat.Gid != 0 {
			t.Errorf("root has wrong gid: %d", stat.Gid)
		}
	}
}

// Test Read calling ReadAll.

type readAll struct{ file }

const hi = "hello, world"

func (readAll) ReadAll(intr fs.Intr) ([]byte, fuse.Error) {
	return []byte(hi), nil
}

func testReadAll(t *testing.T, path string) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	if string(data) != hi {
		t.Errorf("readAll = %q, want %q", data, hi)
	}
}

func TestReadAll(t *testing.T) {
	mnt, err := fstestutil.MountedT(t, childMapFS{"child": readAll{}})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	testReadAll(t, mnt.Dir+"/child")
}

// Test Read.

type readWithHandleRead struct{ file }

func (readWithHandleRead) Read(req *fuse.ReadRequest, resp *fuse.ReadResponse, intr fs.Intr) fuse.Error {
	fuseutil.HandleRead(req, resp, []byte(hi))
	return nil
}

func TestReadAllWithHandleRead(t *testing.T) {
	mnt, err := fstestutil.MountedT(t, childMapFS{"child": readWithHandleRead{}})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	testReadAll(t, mnt.Dir+"/child")
}
