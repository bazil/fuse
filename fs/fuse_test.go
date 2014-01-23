package fs

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
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
