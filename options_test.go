package fuse_test

import (
	"os"
	"runtime"
	"testing"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
)

func init() {
	fstestutil.DebugByDefault()
}

//TODO share
// simpleFS is a trivial FS that just implements the Root method.
type simpleFS struct {
	node fs.Node
}

var _ = fs.FS(simpleFS{})

func (f simpleFS) Root() (fs.Node, fuse.Error) {
	return f.node, nil
}

//TODO share
// dir can be embedded in a struct to make it look like a directory.
type dir struct{}

func (f dir) Attr() fuse.Attr { return fuse.Attr{Mode: os.ModeDir | 0777} }

func TestMountOptionFSName(t *testing.T) {
	t.Parallel()
	const name = "FuseTestMarker"
	mnt, err := fstestutil.MountedT(t, simpleFS{dir{}},
		fuse.FSName(name),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	info, err := fstestutil.GetMountInfo(mnt.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := info.FSName, name; g != e {
		t.Errorf("wrong FSName: %q != %q", g, e)
	}
}

func testMountOptionFSNameEvil(t *testing.T, evil string) {
	t.Parallel()
	var name = "FuseTest" + evil + "Marker"
	mnt, err := fstestutil.MountedT(t, simpleFS{dir{}},
		fuse.FSName(name),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	info, err := fstestutil.GetMountInfo(mnt.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := info.FSName, name; g != e {
		t.Errorf("wrong FSName: %q != %q", g, e)
	}
}

func TestMountOptionFSNameEvilComma(t *testing.T) {
	if runtime.GOOS == "darwin" {
		// see TestMountOptionCommaError for a test that enforces we
		// at least give a nice error, instead of corrupting the mount
		// options
		t.Skip("TODO: OS X gets this wrong, commas in mount options cannot be escaped at all")
	}
	testMountOptionFSNameEvil(t, ",")
}

func TestMountOptionFSNameEvilSpace(t *testing.T) {
	testMountOptionFSNameEvil(t, " ")
}

func TestMountOptionFSNameEvilTab(t *testing.T) {
	testMountOptionFSNameEvil(t, "\t")
}

func TestMountOptionFSNameEvilNewline(t *testing.T) {
	testMountOptionFSNameEvil(t, "\n")
}

func TestMountOptionFSNameEvilBackslash(t *testing.T) {
	testMountOptionFSNameEvil(t, `\`)
}

func TestMountOptionFSNameEvilBackslashDouble(t *testing.T) {
	// catch double-unescaping, if it were to happen
	testMountOptionFSNameEvil(t, `\\`)
}
