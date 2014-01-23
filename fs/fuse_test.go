package fs

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

import (
	"bazil.org/fuse"
	"bazil.org/fuse/syscallx"
)

var fuseRun = flag.String("fuserun", "", "which fuse test to run. runs all if empty.")

func gather(ch chan []byte) []byte {
	var buf []byte
	for b := range ch {
		buf = append(buf, b...)
	}
	return buf
}

// debug adapts fuse.Debug to match t.Log calling convention; due to
// varargs, we can't just assign tb.Log to fuse.Debug
func debug(tb testing.TB) func(msg interface{}) {
	return func(msg interface{}) {
		tb.Log(msg)
	}
}

func TestFuse(t *testing.T) {
	fuse.Debug = debug(t)
	dir, err := ioutil.TempDir("", "fusetest")
	if err != nil {
		t.Fatal(err)
	}

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
	defer fuse.Unmount(dir)

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

var fuseTests = []struct {
	name string
	node interface {
		Node
		test(string, *testing.T)
	}
}{
	{"open", &open{}},
	{"fsyncDir", &fsyncDir{}},
	{"getxattr", &getxattr{}},
	{"getxattrTooSmall", &getxattrTooSmall{}},
	{"getxattrSize", &getxattrSize{}},
	{"listxattr", &listxattr{}},
	{"listxattrTooSmall", &listxattrTooSmall{}},
	{"listxattrSize", &listxattrSize{}},
	{"setxattr", &setxattr{}},
	{"removexattr", &removexattr{}},
}

// TO TEST:
//	Lookup(*LookupRequest, *LookupResponse)
//	Getattr(*GetattrRequest, *GetattrResponse)
//	Attr with explicit inode
//	Setattr(*SetattrRequest, *SetattrResponse)
//	Access(*AccessRequest)
//	Open(*OpenRequest, *OpenResponse)
//	Write(*WriteRequest, *WriteResponse)
//	Flush(*FlushRequest, *FlushResponse)

const hi = "hello, world"

// TODO only used by other tests, at this point
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

type file struct{}
type dir struct{}

func (f file) Attr() fuse.Attr { return fuse.Attr{Mode: 0666} }
func (f dir) Attr() fuse.Attr  { return fuse.Attr{Mode: os.ModeDir | 0777} }

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

// Test open

type openSeen struct {
	dir   bool
	flags fuse.OpenFlags
}

func (s openSeen) String() string {
	return fmt.Sprintf("%T{dir:%v flags:%v}", s, s.dir, s.flags)
}

type open struct {
	file
	seen chan openSeen
}

func (f *open) Open(req *fuse.OpenRequest, resp *fuse.OpenResponse, intr Intr) (Handle, fuse.Error) {
	f.seen <- openSeen{dir: req.Dir, flags: req.Flags}
	// pick a really distinct error, to identify it later
	return nil, fuse.Errno(syscall.ENAMETOOLONG)

}

func (f *open) setup(t *testing.T) {
	f.seen = make(chan openSeen, 1)
}

func (f *open) test(path string, t *testing.T) {
	// node: mode only matters with O_CREATE
	fil, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err == nil {
		t.Error("Open err == nil, expected ENAMETOOLONG")
		fil.Close()
		return
	}

	switch err2 := err.(type) {
	case *os.PathError:
		if err2.Err == syscall.ENAMETOOLONG {
			break
		}
		t.Errorf("unexpected inner error: %#v", err2)
	default:
		t.Errorf("unexpected error: %v", err)
	}

	want := openSeen{dir: false, flags: fuse.OpenFlags(os.O_WRONLY | os.O_APPEND)}
	if runtime.GOOS == "darwin" {
		// osxfuse does not let O_APPEND through at all
		//
		// https://code.google.com/p/macfuse/issues/detail?id=233
		// https://code.google.com/p/macfuse/issues/detail?id=132
		// https://code.google.com/p/macfuse/issues/detail?id=133
		want.flags &^= fuse.OpenFlags(os.O_APPEND)
	}
	if g, e := <-f.seen, want; g != e {
		t.Errorf("open saw %v, want %v", g, e)
		return
	}
}

// Test Fsync on a dir

type fsyncSeen struct {
	flags uint32
	dir   bool
}

type fsyncDir struct {
	dir
	seen chan fsyncSeen
}

func (f *fsyncDir) Fsync(r *fuse.FsyncRequest, intr Intr) fuse.Error {
	f.seen <- fsyncSeen{flags: r.Flags, dir: r.Dir}
	return nil
}

func (f *fsyncDir) setup(t *testing.T) {
	f.seen = make(chan fsyncSeen, 1)
}

func (f *fsyncDir) test(path string, t *testing.T) {
	fil, err := os.Open(path)
	if err != nil {
		t.Errorf("fsyncDir open: %v", err)
		return
	}
	defer fil.Close()
	err = fil.Sync()
	if err != nil {
		t.Errorf("fsyncDir sync: %v", err)
		return
	}

	close(f.seen)
	got := <-f.seen
	want := uint32(0)
	if runtime.GOOS == "darwin" {
		// TODO document the meaning of these flags, figure out why
		// they differ
		want = 1
	}
	if g, e := got.flags, want; g != e {
		t.Errorf("fsyncDir bad flags: %v != %v", g, e)
	}
	if g, e := got.dir, true; g != e {
		t.Errorf("fsyncDir bad dir: %v != %v", g, e)
	}
}

// Test Getxattr

type getxattrSeen struct {
	name string
}

type getxattr struct {
	file
	seen chan getxattrSeen
}

func (f *getxattr) Getxattr(req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse, intr Intr) fuse.Error {
	f.seen <- getxattrSeen{name: req.Name}
	resp.Xattr = []byte("hello, world")
	return nil
}

func (f *getxattr) setup(t *testing.T) {
	f.seen = make(chan getxattrSeen, 1)
}

func (f *getxattr) test(path string, t *testing.T) {
	buf := make([]byte, 8192)
	n, err := syscallx.Getxattr(path, "not-there", buf)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	buf = buf[:n]
	if g, e := string(buf), "hello, world"; g != e {
		t.Errorf("wrong getxattr content: %#v != %#v", g, e)
	}
	close(f.seen)
	seen := <-f.seen
	if g, e := seen.name, "not-there"; g != e {
		t.Errorf("wrong getxattr name: %#v != %#v", g, e)
	}
}

// Test Getxattr that has no space to return value

type getxattrTooSmall struct {
	file
}

func (f *getxattrTooSmall) Getxattr(req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse, intr Intr) fuse.Error {
	resp.Xattr = []byte("hello, world")
	return nil
}

func (f *getxattrTooSmall) test(path string, t *testing.T) {
	buf := make([]byte, 3)
	_, err := syscallx.Getxattr(path, "whatever", buf)
	if err == nil {
		t.Error("Getxattr = nil; want some error")
	}
	if err != syscall.ERANGE {
		t.Errorf("unexpected error: %v", err)
		return
	}
}

// Test Getxattr used to probe result size

type getxattrSize struct {
	file
}

func (f *getxattrSize) Getxattr(req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse, intr Intr) fuse.Error {
	resp.Xattr = []byte("hello, world")
	return nil
}

func (f *getxattrSize) test(path string, t *testing.T) {
	n, err := syscallx.Getxattr(path, "whatever", nil)
	if err != nil {
		t.Errorf("Getxattr unexpected error: %v", err)
		return
	}
	if g, e := n, len("hello, world"); g != e {
		t.Errorf("Getxattr incorrect size: %d != %d", g, e)
	}
}

// Test Listxattr

type listxattr struct {
	file
	seen chan bool
}

func (f *listxattr) Listxattr(req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse, intr Intr) fuse.Error {
	f.seen <- true
	resp.Append("one", "two")
	return nil
}

func (f *listxattr) setup(t *testing.T) {
	f.seen = make(chan bool, 1)
}

func (f *listxattr) test(path string, t *testing.T) {
	buf := make([]byte, 8192)
	n, err := syscallx.Listxattr(path, buf)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	buf = buf[:n]
	if g, e := string(buf), "one\x00two\x00"; g != e {
		t.Errorf("wrong listxattr content: %#v != %#v", g, e)
	}
	close(f.seen)
	seen := <-f.seen
	if g, e := seen, true; g != e {
		t.Errorf("listxattr not seen: %#v != %#v", g, e)
	}
}

// Test Listxattr that has no space to return value

type listxattrTooSmall struct {
	file
}

func (f *listxattrTooSmall) Listxattr(req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse, intr Intr) fuse.Error {
	resp.Xattr = []byte("one\x00two\x00")
	return nil
}

func (f *listxattrTooSmall) test(path string, t *testing.T) {
	buf := make([]byte, 3)
	_, err := syscallx.Listxattr(path, buf)
	if err == nil {
		t.Error("Listxattr = nil; want some error")
	}
	if err != syscall.ERANGE {
		t.Errorf("unexpected error: %v", err)
		return
	}
}

// Test Listxattr used to probe result size

type listxattrSize struct {
	file
}

func (f *listxattrSize) Listxattr(req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse, intr Intr) fuse.Error {
	resp.Xattr = []byte("one\x00two\x00")
	return nil
}

func (f *listxattrSize) test(path string, t *testing.T) {
	n, err := syscallx.Listxattr(path, nil)
	if err != nil {
		t.Errorf("Listxattr unexpected error: %v", err)
		return
	}
	if g, e := n, len("one\x00two\x00"); g != e {
		t.Errorf("Getxattr incorrect size: %d != %d", g, e)
	}
}

// Test Setxattr

type setxattrSeen struct {
	name  string
	flags uint32
	value string
}

type setxattr struct {
	file
	seen chan setxattrSeen
}

func (f *setxattr) Setxattr(req *fuse.SetxattrRequest, intr Intr) fuse.Error {
	f.seen <- setxattrSeen{
		name:  req.Name,
		flags: req.Flags,
		value: string(req.Xattr),
	}
	return nil
}

func (f *setxattr) setup(t *testing.T) {
	f.seen = make(chan setxattrSeen, 1)
}

func (f *setxattr) test(path string, t *testing.T) {
	err := syscallx.Setxattr(path, "greeting", []byte("hello, world"), 0)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	close(f.seen)
	want := setxattrSeen{flags: 0, name: "greeting", value: "hello, world"}
	if g, e := <-f.seen, want; g != e {
		t.Errorf("setxattr saw %v, want %v", g, e)
	}
}

// Test Removexattr

type removexattrSeen struct {
	name string
}

type removexattr struct {
	file
	seen chan removexattrSeen
}

func (f *removexattr) Removexattr(req *fuse.RemovexattrRequest, intr Intr) fuse.Error {
	f.seen <- removexattrSeen{name: req.Name}
	return nil
}

func (f *removexattr) setup(t *testing.T) {
	f.seen = make(chan removexattrSeen, 1)
}

func (f *removexattr) test(path string, t *testing.T) {
	err := syscallx.Removexattr(path, "greeting")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	close(f.seen)
	want := removexattrSeen{name: "greeting"}
	if g, e := <-f.seen, want; g != e {
		t.Errorf("removexattr saw %v, want %v", g, e)
	}
}
