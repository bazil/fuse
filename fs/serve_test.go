package fs_test

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
	"bazil.org/fuse/fs/fstestutil/record"
	"bazil.org/fuse/fuseutil"
	"io/ioutil"
	"log"
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

// simpleFS is a trivial FS that just implements the Root method.
type simpleFS struct {
	node fs.Node
}

var _ = fs.FS(simpleFS{})

func (f simpleFS) Root() (fs.Node, fuse.Error) {
	return f.node, nil
}

// file can be embedded in a struct to make it look like a file.
type file struct{}

func (f file) Attr() fuse.Attr { return fuse.Attr{Mode: 0666} }

// dir can be embedded in a struct to make it look like a directory.
type dir struct{}

func (f dir) Attr() fuse.Attr { return fuse.Attr{Mode: os.ModeDir | 0777} }

// symlink can be embedded in a struct to make it look like a symlink.
type symlink struct {
	target string
}

func (f symlink) Attr() fuse.Attr { return fuse.Attr{Mode: os.ModeSymlink | 0666} }

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

// Test Release.

type release struct {
	file
	record.ReleaseWaiter
}

func TestRelease(t *testing.T) {
	r := &release{}
	mnt, err := fstestutil.MountedT(t, childMapFS{"child": r})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	f, err := os.Open(mnt.Dir + "/child")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if !r.WaitForRelease(1 * time.Second) {
		t.Error("Close did not Release in time")
	}
}

// Test Write calling basic Write, with an fsync thrown in too.

type write struct {
	file
	record.Writes
	record.Fsyncs
}

func TestWrite(t *testing.T) {
	w := &write{}
	mnt, err := fstestutil.MountedT(t, childMapFS{"child": w})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	f, err := os.Create(mnt.Dir + "/child")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	n, err := f.Write([]byte(hi))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(hi) {
		t.Fatalf("short write; n=%d; hi=%d", n, len(hi))
	}

	err = syscall.Fsync(int(f.Fd()))
	if err != nil {
		t.Fatalf("Fsync = %v", err)
	}
	if !w.RecordedFsync() {
		t.Errorf("never received expected fsync call")
	}

	err = f.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := string(w.RecordedWriteData()); got != hi {
		t.Errorf("write = %q, want %q", got, hi)
	}
}

// Test Write calling Setattr+Write+Flush.

type writeTruncateFlush struct {
	file
	record.Writes
	record.Setattrs
	record.Flushes
}

func TestWriteTruncateFlush(t *testing.T) {
	w := &writeTruncateFlush{}
	mnt, err := fstestutil.MountedT(t, childMapFS{"child": w})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	err = ioutil.WriteFile(mnt.Dir+"/child", []byte(hi), 0666)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !w.RecordedSetattr() {
		t.Errorf("writeTruncateFlush expected Setattr")
	}
	if !w.RecordedFlush() {
		t.Errorf("writeTruncateFlush expected Setattr")
	}
	if got := string(w.RecordedWriteData()); got != hi {
		t.Errorf("writeTruncateFlush = %q, want %q", got, hi)
	}
}

// Test Mkdir.

type mkdir1 struct {
	dir
	record.Mkdirs
}

func (f *mkdir1) Mkdir(req *fuse.MkdirRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	f.Mkdirs.Mkdir(req, intr)
	return &mkdir1{}, nil
}

func TestMkdir(t *testing.T) {
	f := &mkdir1{}
	mnt, err := fstestutil.MountedT(t, simpleFS{f})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	// uniform umask needed to make os.Mkdir's mode into something
	// reproducible
	defer syscall.Umask(syscall.Umask(0022))
	err = os.Mkdir(mnt.Dir+"/foo", 0771)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := fuse.MkdirRequest{Name: "foo", Mode: os.ModeDir | 0751}
	if g, e := f.RecordedMkdir(), want; g != e {
		t.Errorf("mkdir saw %+v, want %+v", g, e)
	}
}

// Test Create (and fsync)

type create1file struct {
	file
	record.Fsyncs
}

type create1 struct {
	dir
	f create1file
}

func (f *create1) Create(req *fuse.CreateRequest, resp *fuse.CreateResponse, intr fs.Intr) (fs.Node, fs.Handle, fuse.Error) {
	if req.Name != "foo" {
		log.Printf("ERROR create1.Create unexpected name: %q\n", req.Name)
		return nil, nil, fuse.EPERM
	}
	flags := req.Flags
	// OS X does not pass O_TRUNC here, Linux does; as this is a
	// Create, that's acceptable
	flags &^= fuse.OpenFlags(os.O_TRUNC)
	if g, e := flags, fuse.OpenFlags(os.O_CREATE|os.O_RDWR); g != e {
		log.Printf("ERROR create1.Create unexpected flags: %v != %v\n", g, e)
		return nil, nil, fuse.EPERM
	}
	if g, e := req.Mode, os.FileMode(0644); g != e {
		log.Printf("ERROR create1.Create unexpected mode: %v != %v\n", g, e)
		return nil, nil, fuse.EPERM
	}
	return &f.f, &f.f, nil
}

func TestCreate(t *testing.T) {
	f := &create1{}
	mnt, err := fstestutil.MountedT(t, simpleFS{f})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	// uniform umask needed to make os.Create's 0666 into something
	// reproducible
	defer syscall.Umask(syscall.Umask(0022))
	ff, err := os.Create(mnt.Dir + "/foo")
	if err != nil {
		t.Fatalf("create1 WriteFile: %v", err)
	}

	err = syscall.Fsync(int(ff.Fd()))
	if err != nil {
		t.Fatalf("Fsync = %v", err)
	}

	if !f.f.RecordedFsync() {
		t.Errorf("never received expected fsync call")
	}

	ff.Close()
}

// Test Create + Write + Remove

type create3file struct {
	file
	record.Writes
}

type create3 struct {
	dir
	f          create3file
	fooCreated record.MarkRecorder
	fooRemoved record.MarkRecorder
}

func (f *create3) Create(req *fuse.CreateRequest, resp *fuse.CreateResponse, intr fs.Intr) (fs.Node, fs.Handle, fuse.Error) {
	if req.Name != "foo" {
		log.Printf("ERROR create3.Create unexpected name: %q\n", req.Name)
		return nil, nil, fuse.EPERM
	}
	f.fooCreated.Mark()
	return &f.f, &f.f, nil
}

func (f *create3) Lookup(name string, intr fs.Intr) (fs.Node, fuse.Error) {
	if f.fooCreated.Recorded() && !f.fooRemoved.Recorded() && name == "foo" {
		return &f.f, nil
	}
	return nil, fuse.ENOENT
}

func (f *create3) Remove(r *fuse.RemoveRequest, intr fs.Intr) fuse.Error {
	if f.fooCreated.Recorded() && !f.fooRemoved.Recorded() &&
		r.Name == "foo" && !r.Dir {
		f.fooRemoved.Mark()
		return nil
	}
	return fuse.ENOENT
}

func TestCreateWriteRemove(t *testing.T) {
	f := &create3{}
	mnt, err := fstestutil.MountedT(t, simpleFS{f})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	err = ioutil.WriteFile(mnt.Dir+"/foo", []byte(hi), 0666)
	if err != nil {
		t.Fatalf("create3 WriteFile: %v", err)
	}
	if got := string(f.f.RecordedWriteData()); got != hi {
		t.Fatalf("create3 write = %q, want %q", got, hi)
	}

	err = os.Remove(mnt.Dir + "/foo")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	err = os.Remove(mnt.Dir + "/foo")
	if err == nil {
		t.Fatalf("second Remove = nil; want some error")
	}
}

// Test symlink + readlink

// is a Node that is a symlink to target
type symlink1link struct {
	symlink
	target string
}

func (f symlink1link) Readlink(*fuse.ReadlinkRequest, fs.Intr) (string, fuse.Error) {
	return f.target, nil
}

type symlink1 struct {
	dir
	record.Symlinks
}

func (f *symlink1) Symlink(req *fuse.SymlinkRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	f.Symlinks.Symlink(req, intr)
	return symlink1link{target: req.Target}, nil
}

func TestSymlink(t *testing.T) {
	f := &symlink1{}
	mnt, err := fstestutil.MountedT(t, simpleFS{f})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	const target = "/some-target"

	err = os.Symlink(target, mnt.Dir+"/symlink.file")
	if err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}

	want := fuse.SymlinkRequest{NewName: "symlink.file", Target: target}
	if g, e := f.RecordedSymlink(), want; g != e {
		t.Errorf("symlink saw %+v, want %+v", g, e)
	}

	gotName, err := os.Readlink(mnt.Dir + "/symlink.file")
	if err != nil {
		t.Fatalf("os.Readlink: %v", err)
	}
	if gotName != target {
		t.Errorf("os.Readlink = %q; want %q", gotName, target)
	}
}

// Test link

type link1 struct {
	dir
	record.Links
}

func (f *link1) Lookup(name string, intr fs.Intr) (fs.Node, fuse.Error) {
	if name == "old" {
		return file{}, nil
	}
	return nil, fuse.ENOENT
}

func (f *link1) Link(r *fuse.LinkRequest, old fs.Node, intr fs.Intr) (fs.Node, fuse.Error) {
	f.Links.Link(r, old, intr)
	return file{}, nil
}

func TestLink(t *testing.T) {
	f := &link1{}
	mnt, err := fstestutil.MountedT(t, simpleFS{f})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	err = os.Link(mnt.Dir+"/old", mnt.Dir+"/new")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}

	got := f.RecordedLink()
	want := fuse.LinkRequest{
		NewName: "new",
		// unpredictable
		OldNode: got.OldNode,
	}
	if g, e := got, want; g != e {
		t.Fatalf("link saw %+v, want %+v", g, e)
	}
}
