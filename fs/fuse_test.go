// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fs

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fuseutil"
)

var fuseRun = flag.String("fuserun", "", "which fuse test to run. runs all if empty.")

// umount tries its best to unmount dir.
func umount(dir string) {
	err := exec.Command("umount", dir).Run()
	if err != nil && runtime.GOOS == "linux" {
		exec.Command("/bin/fusermount", "-u", dir).Run()
	}
}

func gather(ch chan []byte) []byte {
	var buf []byte
	for b := range ch {
		buf = append(buf, b...)
	}
	return buf
}

type badRootFS struct{}

func (badRootFS) Root() (Node, fuse.Error) {
	// pick a really distinct error, to identify it later
	return nil, fuse.Errno(syscall.EUCLEAN)
}

func TestRootErr(t *testing.T) {
	fuse.Debugf = log.Printf
	dir, err := ioutil.TempDir("", "fusetest")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0777)

	c, err := fuse.Mount(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer umount(dir)

	ch := make(chan error, 1)
	go func() {
		ch <- Serve(c, badRootFS{})
	}()

	select {
	case err := <-ch:
		if err.Error() != "cannot obtain root node: structure needs cleaning" {
			t.Errorf("Unexpected error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Serve did not return an error as expected, aborting")
	}
}

type testStatFS struct{}

func (f testStatFS) Root() (Node, fuse.Error) {
	return f, nil
}

func (f testStatFS) Attr() fuse.Attr {
	return fuse.Attr{Inode: 1, Mode: os.ModeDir | 0777}
}

func (f testStatFS) Statfs(req *fuse.StatfsRequest, resp *fuse.StatfsResponse, int Intr) fuse.Error {
	resp.Blocks = 42
	resp.Namelen = 13
	return nil
}

func TestStatfs(t *testing.T) {
	fuse.Debugf = log.Printf
	dir, err := ioutil.TempDir("", "fusetest")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0777)

	c, err := fuse.Mount(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer umount(dir)

	go func() {
		err := Serve(c, testStatFS{})
		if err != nil {
			fmt.Printf("SERVE ERROR: %v\n", err)
		}
	}()

	waitForMount_inode1(t, dir)

	{
		var st syscall.Statfs_t
		err = syscall.Statfs(dir, &st)
		if err != nil {
			t.Errorf("Statfs failed: %v", err)
		}
		t.Logf("Statfs got: %#v", st)
		if g, e := st.Blocks, uint64(42); g != e {
			t.Errorf("got Blocks = %q; want %q", g, e)
		}
		if g, e := st.Namelen, int64(13); g != e {
			t.Errorf("got Namelen = %q; want %q", g, e)
		}
	}

	{
		var st syscall.Statfs_t
		f, err := os.Open(dir)
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
		if g, e := st.Namelen, int64(13); g != e {
			t.Errorf("got Namelen = %q; want %q", g, e)
		}
	}

}

func TestFuse(t *testing.T) {
	fuse.Debugf = log.Printf
	dir, err := ioutil.TempDir("", "fusetest")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0777)

	for _, tt := range fuseTests {
		if *fuseRun == "" || *fuseRun == tt.name {
			if st, ok := tt.node.(interface {
				setup(*testing.T)
			}); ok {
				t.Logf("setting up %T", tt.node)
				st.setup(t)
			}
		}
	}

	c, err := fuse.Mount(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer umount(dir)

	go func() {
		err := Serve(c, testFS{})
		if err != nil {
			fmt.Printf("SERVE ERROR: %v\n", err)
		}
	}()

	waitForMount(t, dir)

	for _, tt := range fuseTests {
		if *fuseRun == "" || *fuseRun == tt.name {
			t.Logf("running %T", tt.node)
			tt.node.test(dir+"/"+tt.name, t)
		}
	}
}

func waitForMount(t *testing.T, dir string) {
	// Filename to wait for in dir:
	probeEntry := *fuseRun
	if probeEntry == "" {
		probeEntry = fuseTests[0].name
	}
	for tries := 0; tries < 100; tries++ {
		_, err := os.Stat(dir + "/" + probeEntry)
		if err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("mount did not work")
}

// TODO maybe wait for fstype to change to FUse (verify it's not fuse to begin with)
func waitForMount_inode1(t *testing.T, dir string) {
	for tries := 0; tries < 100; tries++ {
		fi, err := os.Stat(dir)
		if err == nil {
			if si, ok := fi.Sys().(*syscall.Stat_t); ok {
				if si.Ino == 1 {
					return
				}
				t.Logf("waiting for root: %v", si.Ino)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("mount did not work")
}

var fuseTests = []struct {
	name string
	node interface {
		Node
		test(string, *testing.T)
	}
}{
	{"root", &root{}},
	{"readAll", readAll{}},
	{"readAll1", &readAll1{}},
	{"write", &write{}},
	{"writeAll", &writeAll{}},
	{"writeAll2", &writeAll2{}},
	{"release", &release{}},
	{"mkdir1", &mkdir1{}},
	{"create1", &create1{}},
	{"create2", &create2{}},
	{"create3", &create3{}},
	{"symlink1", &symlink1{}},
	{"link1", &link1{}},
	{"rename1", &rename1{}},
	{"mknod1", &mknod1{}},
	{"dataHandle", dataHandleTest{}},
	{"interrupt", &interrupt{}},
	{"truncate42", &truncate{toSize: 42}},
	{"truncate0", &truncate{toSize: 0}},
	{"ftruncate42", &ftruncate{toSize: 42}},
	{"ftruncate0", &ftruncate{toSize: 0}},
	{"truncateWithOpen", &truncateWithOpen{}},
}

// TO TEST:
//	Lookup(*LookupRequest, *LookupResponse)
//	Getattr(*GetattrRequest, *GetattrResponse)
//	Attr with explicit inode
//	Setattr(*SetattrRequest, *SetattrResponse)
//	Access(*AccessRequest)
//	Open(*OpenRequest, *OpenResponse)
//	Getxattr, Setxattr, Listxattr, Removexattr
//	Write(*WriteRequest, *WriteResponse)
//	Flush(*FlushRequest, *FlushResponse)

// Test Stat of root.

type root struct {
	dir
}

func (f *root) test(path string, t *testing.T) {
	fi, err := os.Stat(path + "/..")
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

func (readAll) ReadAll(intr Intr) ([]byte, fuse.Error) {
	return []byte(hi), nil
}

func (readAll) test(path string, t *testing.T) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		t.Errorf("readAll: %v", err)
		return
	}
	if string(data) != hi {
		t.Errorf("readAll = %q, want %q", data, hi)
	}
}

// Test Read.

type readAll1 struct{ file }

func (readAll1) Read(req *fuse.ReadRequest, resp *fuse.ReadResponse, intr Intr) fuse.Error {
	fuseutil.HandleRead(req, resp, []byte(hi))
	return nil
}

func (readAll1) test(path string, t *testing.T) {
	readAll{}.test(path, t)
}

// Test Write calling basic Write, with an fsync thrown in too.

type write struct {
	file
	seen struct {
		data  chan []byte
		fsync chan bool
	}
}

func (w *write) Write(req *fuse.WriteRequest, resp *fuse.WriteResponse, intr Intr) fuse.Error {
	w.seen.data <- req.Data
	resp.Size = len(req.Data)
	return nil
}

func (w *write) Fsync(r *fuse.FsyncRequest, intr Intr) fuse.Error {
	w.seen.fsync <- true
	return nil
}

func (w *write) Release(r *fuse.ReleaseRequest, intr Intr) fuse.Error {
	close(w.seen.data)
	return nil
}

func (w *write) setup(t *testing.T) {
	w.seen.data = make(chan []byte, 10)
	w.seen.fsync = make(chan bool, 1)
}

func (w *write) test(path string, t *testing.T) {
	log.Printf("pre-write Create")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	log.Printf("pre-write Write")
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
	if !<-w.seen.fsync {
		t.Errorf("never received expected fsync call")
	}

	log.Printf("pre-write Close")
	err = f.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	log.Printf("post-write Close")
	if got := string(gather(w.seen.data)); got != hi {
		t.Errorf("writeAll = %q, want %q", got, hi)
	}
}

// Test Write calling WriteAll.

type writeAll struct {
	file
	seen struct {
		data  chan []byte
		fsync chan bool
	}
}

func (w *writeAll) Fsync(r *fuse.FsyncRequest, intr Intr) fuse.Error {
	w.seen.fsync <- true
	return nil
}

func (w *writeAll) WriteAll(data []byte, intr Intr) fuse.Error {
	w.seen.data <- data
	return nil
}

func (w *writeAll) Release(r *fuse.ReleaseRequest, intr Intr) fuse.Error {
	close(w.seen.data)
	return nil
}

func (w *writeAll) setup(t *testing.T) {
	w.seen.data = make(chan []byte, 10)
	w.seen.fsync = make(chan bool, 1)
}

func (w *writeAll) test(path string, t *testing.T) {
	err := ioutil.WriteFile(path, []byte(hi), 0666)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
		return
	}
	if got := string(gather(w.seen.data)); got != hi {
		t.Errorf("writeAll = %q, want %q", got, hi)
	}
}

// Test Write calling Setattr+Write+Flush.

type writeAll2 struct {
	file
	seen struct {
		data    chan []byte
		setattr chan bool
		flush   chan bool
	}
}

func (w *writeAll2) Setattr(req *fuse.SetattrRequest, resp *fuse.SetattrResponse, intr Intr) fuse.Error {
	w.seen.setattr <- true
	return nil
}

func (w *writeAll2) Flush(req *fuse.FlushRequest, intr Intr) fuse.Error {
	w.seen.flush <- true
	return nil
}

func (w *writeAll2) Write(req *fuse.WriteRequest, resp *fuse.WriteResponse, intr Intr) fuse.Error {
	w.seen.data <- req.Data
	resp.Size = len(req.Data)
	return nil
}

func (w *writeAll2) Release(r *fuse.ReleaseRequest, intr Intr) fuse.Error {
	close(w.seen.data)
	return nil
}

func (w *writeAll2) setup(t *testing.T) {
	w.seen.data = make(chan []byte, 100)
	w.seen.setattr = make(chan bool, 1)
	w.seen.flush = make(chan bool, 1)
}

func (w *writeAll2) test(path string, t *testing.T) {
	err := ioutil.WriteFile(path, []byte(hi), 0666)
	if err != nil {
		t.Errorf("WriteFile: %v", err)
		return
	}
	if !<-w.seen.setattr {
		t.Errorf("writeAll expected Setattr")
	}
	if !<-w.seen.flush {
		t.Errorf("writeAll expected Setattr")
	}
	if got := string(gather(w.seen.data)); got != hi {
		t.Errorf("writeAll = %q, want %q", got, hi)
	}
}

// Test Mkdir.

type mkdir1 struct {
	dir
	seen struct {
		name chan string
	}
}

func (f *mkdir1) Mkdir(req *fuse.MkdirRequest, intr Intr) (Node, fuse.Error) {
	f.seen.name <- req.Name
	return &mkdir1{}, nil
}

func (f *mkdir1) setup(t *testing.T) {
	f.seen.name = make(chan string, 1)
}

func (f *mkdir1) test(path string, t *testing.T) {
	err := os.Mkdir(path+"/foo", 0777)
	if err != nil {
		t.Error(err)
		return
	}
	if <-f.seen.name != "foo" {
		t.Error(err)
		return
	}
}

// Test Create (and fsync)

type create1 struct {
	dir
	f writeAll
}

func (f *create1) Create(req *fuse.CreateRequest, resp *fuse.CreateResponse, intr Intr) (Node, Handle, fuse.Error) {
	if req.Name != "foo" {
		log.Printf("ERROR create1.Create unexpected name: %q\n", req.Name)
		return nil, nil, fuse.EPERM
	}
	return &f.f, &f.f, nil
}

func (f *create1) setup(t *testing.T) {
	f.f.setup(t)
}

func (f *create1) test(path string, t *testing.T) {
	ff, err := os.Create(path + "/foo")
	if err != nil {
		t.Errorf("create1 WriteFile: %v", err)
		return
	}

	err = syscall.Fsync(int(ff.Fd()))
	if err != nil {
		t.Fatalf("Fsync = %v", err)
	}

	if !<-f.f.seen.fsync {
		t.Errorf("never received expected fsync call")
	}

	ff.Close()
}

// Test Create + WriteAll + Remove

type create2 struct {
	dir
	f         writeAll
	fooExists bool
}

func (f *create2) Create(req *fuse.CreateRequest, resp *fuse.CreateResponse, intr Intr) (Node, Handle, fuse.Error) {
	if req.Name != "foo" {
		log.Printf("ERROR create2.Create unexpected name: %q\n", req.Name)
		return nil, nil, fuse.EPERM
	}
	return &f.f, &f.f, nil
}

func (f *create2) Lookup(name string, intr Intr) (Node, fuse.Error) {
	if f.fooExists && name == "foo" {
		return file{}, nil
	}
	return nil, fuse.ENOENT
}

func (f *create2) Remove(r *fuse.RemoveRequest, intr Intr) fuse.Error {
	if f.fooExists && r.Name == "foo" && !r.Dir {
		f.fooExists = false
		return nil
	}
	return fuse.ENOENT
}

func (f *create2) setup(t *testing.T) {
	f.f.setup(t)
}

func (f *create2) test(path string, t *testing.T) {
	err := ioutil.WriteFile(path+"/foo", []byte(hi), 0666)
	if err != nil {
		t.Fatalf("create2 WriteFile: %v", err)
	}
	if got := string(gather(f.f.seen.data)); got != hi {
		t.Fatalf("create2 writeAll = %q, want %q", got, hi)
	}

	f.fooExists = true
	log.Printf("pre-Remove")
	err = os.Remove(path + "/foo")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	err = os.Remove(path + "/foo")
	if err == nil {
		t.Fatalf("second Remove = nil; want some error")
	}
}

// Test Create + WriteAll + Remove

type create3 struct {
	dir
	f         write
	fooExists bool
}

func (f *create3) Create(req *fuse.CreateRequest, resp *fuse.CreateResponse, intr Intr) (Node, Handle, fuse.Error) {
	if req.Name != "foo" {
		log.Printf("ERROR create3.Create unexpected name: %q\n", req.Name)
		return nil, nil, fuse.EPERM
	}
	return &f.f, &f.f, nil
}

func (f *create3) Lookup(name string, intr Intr) (Node, fuse.Error) {
	if f.fooExists && name == "foo" {
		return file{}, nil
	}
	return nil, fuse.ENOENT
}

func (f *create3) Remove(r *fuse.RemoveRequest, intr Intr) fuse.Error {
	if f.fooExists && r.Name == "foo" && !r.Dir {
		f.fooExists = false
		return nil
	}
	return fuse.ENOENT
}

func (f *create3) setup(t *testing.T) {
	f.f.setup(t)
}

func (f *create3) test(path string, t *testing.T) {
	err := ioutil.WriteFile(path+"/foo", []byte(hi), 0666)
	if err != nil {
		t.Fatalf("create3 WriteFile: %v", err)
	}
	if got := string(gather(f.f.seen.data)); got != hi {
		t.Fatalf("create3 writeAll = %q, want %q", got, hi)
	}

	f.fooExists = true
	log.Printf("pre-Remove")
	err = os.Remove(path + "/foo")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	err = os.Remove(path + "/foo")
	if err == nil {
		t.Fatalf("second Remove = nil; want some error")
	}
}

// Test symlink + readlink

type symlink1 struct {
	dir
	seen struct {
		req chan *fuse.SymlinkRequest
	}
}

func (f *symlink1) Symlink(req *fuse.SymlinkRequest, intr Intr) (Node, fuse.Error) {
	f.seen.req <- req
	return symlink{target: req.Target}, nil
}

func (f *symlink1) setup(t *testing.T) {
	f.seen.req = make(chan *fuse.SymlinkRequest, 1)
}

func (f *symlink1) test(path string, t *testing.T) {
	const target = "/some-target"

	err := os.Symlink(target, path+"/symlink.file")
	if err != nil {
		t.Errorf("os.Symlink: %v", err)
		return
	}

	req := <-f.seen.req

	if req.NewName != "symlink.file" {
		t.Errorf("symlink newName = %q; want %q", req.NewName, "symlink.file")
	}
	if req.Target != target {
		t.Errorf("symlink target = %q; want %q", req.Target, target)
	}

	gotName, err := os.Readlink(path + "/symlink.file")
	if err != nil {
		t.Errorf("os.Readlink: %v", err)
		return
	}
	if gotName != target {
		t.Errorf("os.Readlink = %q; want %q", gotName, target)
	}
}

// Test link

type link1 struct {
	dir
	seen struct {
		newName chan string
	}
}

func (f *link1) Lookup(name string, intr Intr) (Node, fuse.Error) {
	if name == "old" {
		return file{}, nil
	}
	return nil, fuse.ENOENT
}

func (f *link1) Link(r *fuse.LinkRequest, old Node, intr Intr) (Node, fuse.Error) {
	f.seen.newName <- r.NewName
	return file{}, nil
}

func (f *link1) setup(t *testing.T) {
	f.seen.newName = make(chan string, 1)
}

func (f *link1) test(path string, t *testing.T) {
	err := os.Link(path+"/old", path+"/new")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got := <-f.seen.newName; got != "new" {
		t.Fatalf("saw Link for newName %q; want %q", got, "new")
	}
}

// Test Rename

type rename1 struct {
	dir
	renames int
}

func (f *rename1) Lookup(name string, intr Intr) (Node, fuse.Error) {
	if name == "old" {
		return file{}, nil
	}
	return nil, fuse.ENOENT
}

func (f *rename1) Rename(r *fuse.RenameRequest, newDir Node, intr Intr) fuse.Error {
	if r.OldName == "old" && r.NewName == "new" && newDir == f {
		f.renames++
		return nil
	}
	return fuse.EIO
}

func (f *rename1) test(path string, t *testing.T) {
	err := os.Rename(path+"/old", path+"/new")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if f.renames != 1 {
		t.Fatalf("expected rename didn't happen")
	}
	err = os.Rename(path+"/old2", path+"/new2")
	if err == nil {
		t.Fatal("expected error on second Rename; got nil")
	}
}

// Test Release.

type release struct {
	file
	seen struct {
		did chan bool
	}
}

func (r *release) Release(*fuse.ReleaseRequest, Intr) fuse.Error {
	r.seen.did <- true
	return nil
}

func (r *release) setup(t *testing.T) {
	r.seen.did = make(chan bool, 1)
}

func (r *release) test(path string, t *testing.T) {
	f, err := os.Open(path)
	if err != nil {
		t.Error(err)
		return
	}
	f.Close()
	time.Sleep(1 * time.Second)
	if !<-r.seen.did {
		t.Error("Close did not Release")
	}
}

// Test mknod

type mknod1 struct {
	dir
	gotr *fuse.MknodRequest
}

func (f *mknod1) Mknod(r *fuse.MknodRequest, intr Intr) (Node, fuse.Error) {
	f.gotr = r
	return fifo{}, nil
}

func (f *mknod1) test(path string, t *testing.T) {
	if os.Getuid() != 0 {
		t.Logf("skipping unless root")
		return
	}
	defer syscall.Umask(syscall.Umask(0))
	err := syscall.Mknod(path+"/node", syscall.S_IFIFO|0666, 123)
	if err != nil {
		t.Fatalf("Mknod: %v", err)
	}
	if f.gotr == nil {
		t.Fatalf("no recorded MknodRequest")
	}
	if g, e := f.gotr.Name, "node"; g != e {
		t.Errorf("got Name = %q; want %q", g, e)
	}
	if g, e := f.gotr.Rdev, uint32(123); g != e {
		if runtime.GOOS == "linux" {
			// Linux fuse doesn't echo back the rdev if the node
			// isn't a device (we're using a FIFO here, as that
			// bit is portable.)
		} else {
			t.Errorf("got Rdev = %v; want %v", g, e)
		}
	}
	if g, e := f.gotr.Mode, os.FileMode(os.ModeNamedPipe|0666); g != e {
		t.Errorf("got Mode = %v; want %v", g, e)
	}
	t.Logf("Got request: %#v", f.gotr)
}

type file struct{}
type dir struct{}
type fifo struct{}
type symlink struct {
	target string
}

func (f file) Attr() fuse.Attr    { return fuse.Attr{Mode: 0666} }
func (f dir) Attr() fuse.Attr     { return fuse.Attr{Mode: os.ModeDir | 0777} }
func (f fifo) Attr() fuse.Attr    { return fuse.Attr{Mode: os.ModeNamedPipe | 0666} }
func (f symlink) Attr() fuse.Attr { return fuse.Attr{Mode: os.ModeSymlink | 0666} }

func (f symlink) Readlink(*fuse.ReadlinkRequest, Intr) (string, fuse.Error) {
	return f.target, nil
}

type testFS struct{}

func (testFS) Root() (Node, fuse.Error) {
	return testFS{}, nil
}

func (testFS) Attr() fuse.Attr {
	return fuse.Attr{Inode: 1, Mode: os.ModeDir | 0555}
}

func (testFS) Lookup(name string, intr Intr) (Node, fuse.Error) {
	for _, tt := range fuseTests {
		if tt.name == name {
			return tt.node, nil
		}
	}
	return nil, fuse.ENOENT
}

func (testFS) ReadDir(intr Intr) ([]fuse.Dirent, fuse.Error) {
	var dirs []fuse.Dirent
	for _, tt := range fuseTests {
		if *fuseRun == "" || *fuseRun == tt.name {
			log.Printf("Readdir; adding %q", tt.name)
			dirs = append(dirs, fuse.Dirent{Name: tt.name})
		}
	}
	return dirs, nil
}

// Test Read served with DataHandle.

type dataHandleTest struct {
	file
}

func (dataHandleTest) Open(*fuse.OpenRequest, *fuse.OpenResponse, Intr) (Handle, fuse.Error) {
	return DataHandle([]byte(hi)), nil
}

func (dataHandleTest) test(path string, t *testing.T) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		t.Errorf("readAll: %v", err)
		return
	}
	if string(data) != hi {
		t.Errorf("readAll = %q, want %q", data, hi)
	}
}

// Test interrupt

type interrupt struct {
	file

	// closed to signal we have a read hanging
	hanging chan struct{}
}

func (it *interrupt) Read(req *fuse.ReadRequest, resp *fuse.ReadResponse, intr Intr) fuse.Error {
	if it.hanging == nil {
		fuseutil.HandleRead(req, resp, []byte("don't read this outside of the test"))
		return nil
	}

	close(it.hanging)
	<-intr
	return fuse.Errno(syscall.EINTR)
}

func (it *interrupt) test(path string, t *testing.T) {
	it.hanging = make(chan struct{})

	// start a subprocess that can hang until signaled
	cmd := exec.Command("cat", path)

	err := cmd.Start()
	if err != nil {
		t.Errorf("interrupt: cannot start cat: %v", err)
		return
	}

	// try to clean up if child is still alive when returning
	defer cmd.Process.Kill()

	// wait till we're sure it's hanging in read
	<-it.hanging

	err = cmd.Process.Signal(os.Interrupt)
	if err != nil {
		t.Errorf("interrupt: cannot interrupt cat: %v", err)
		return
	}

	p, err := cmd.Process.Wait()
	if err != nil {
		t.Errorf("interrupt: cat bork: %v", err)
		return
	}
	switch ws := p.Sys().(type) {
	case syscall.WaitStatus:
		if ws.CoreDump() {
			t.Errorf("interrupt: didn't expect cat to dump core: %v", ws)
		}

		if ws.Exited() {
			t.Errorf("interrupt: didn't expect cat to exit normally: %v", ws)
		}

		if !ws.Signaled() {
			t.Errorf("interrupt: expected cat to get a signal: %v", ws)
		} else {
			if ws.Signal() != os.Interrupt {
				t.Errorf("interrupt: cat got wrong signal: %v", ws)
			}
		}
	default:
		t.Logf("interrupt: this platform has no test coverage")
	}
}

// Test truncate

type truncate struct {
	toSize int64

	file
	gotr *fuse.SetattrRequest
}

// present purely to trigger bugs in WriteAll logic
func (*truncate) WriteAll(data []byte, intr Intr) fuse.Error {
	return nil
}

func (f *truncate) Setattr(req *fuse.SetattrRequest, resp *fuse.SetattrResponse, intr Intr) fuse.Error {
	f.gotr = req
	return nil
}

func (f *truncate) test(path string, t *testing.T) {
	err := os.Truncate(path, f.toSize)
	if err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if f.gotr == nil {
		t.Fatalf("no recorded SetattrRequest")
	}
	if g, e := f.gotr.Size, uint64(f.toSize); g != e {
		t.Errorf("got Size = %q; want %q", g, e)
	}
	if g, e := f.gotr.Valid&^fuse.SetattrLockOwner, fuse.SetattrSize; g != e {
		t.Errorf("got Valid = %q; want %q", g, e)
	}
	t.Logf("Got request: %#v", f.gotr)
}

// Test ftruncate

type ftruncate struct {
	toSize int64

	file
	gotr *fuse.SetattrRequest
}

// present purely to trigger bugs in WriteAll logic
func (*ftruncate) WriteAll(data []byte, intr Intr) fuse.Error {
	return nil
}

func (f *ftruncate) Setattr(req *fuse.SetattrRequest, resp *fuse.SetattrResponse, intr Intr) fuse.Error {
	f.gotr = req
	return nil
}

func (f *ftruncate) test(path string, t *testing.T) {
	{
		fil, err := os.OpenFile(path, os.O_WRONLY, 0666)
		if err != nil {
			t.Error(err)
			return
		}
		defer fil.Close()

		err = fil.Truncate(f.toSize)
		if err != nil {
			t.Fatalf("Ftruncate: %v", err)
		}
	}
	if f.gotr == nil {
		t.Fatalf("no recorded SetattrRequest")
	}
	if g, e := f.gotr.Size, uint64(f.toSize); g != e {
		t.Errorf("got Size = %q; want %q", g, e)
	}
	if g, e := f.gotr.Valid&^fuse.SetattrLockOwner, fuse.SetattrHandle|fuse.SetattrSize; g != e {
		t.Errorf("got Valid = %q; want %q", g, e)
	}
	t.Logf("Got request: %#v", f.gotr)
}

// Test opening existing file truncates

type truncateWithOpen struct {
	file
	gotr *fuse.SetattrRequest
}

// present purely to trigger bugs in WriteAll logic
func (*truncateWithOpen) WriteAll(data []byte, intr Intr) fuse.Error {
	return nil
}

func (f *truncateWithOpen) Setattr(req *fuse.SetattrRequest, resp *fuse.SetattrResponse, intr Intr) fuse.Error {
	f.gotr = req
	return nil
}

func (f *truncateWithOpen) test(path string, t *testing.T) {
	{
		fil, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0666)
		if err != nil {
			t.Error(err)
			return
		}
		fil.Close()

		if err != nil {
			t.Fatalf("TruncateWithOpen: %v", err)
		}
	}
	if f.gotr == nil {
		t.Fatalf("no recorded SetattrRequest")
	}
	if g, e := f.gotr.Size, uint64(0); g != e {
		t.Errorf("got Size = %q; want %q", g, e)
	}
	if g, e := f.gotr.Valid&^fuse.SetattrLockOwner, fuse.SetattrSize; g != e {
		t.Errorf("got Valid = %q; want %q", g, e)
	}
	t.Logf("Got request: %#v", f.gotr)
}
