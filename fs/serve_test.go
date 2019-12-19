package fs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
	"bazil.org/fuse/fs/fstestutil/record"
	"bazil.org/fuse/fs/fstestutil/spawntest"
	"bazil.org/fuse/fs/fstestutil/spawntest/httpjson"
	"bazil.org/fuse/fuseutil"
	"bazil.org/fuse/syscallx"
)

func maybeParallel(t *testing.T) {
	// t.Parallel()
}

var helpers spawntest.Registry

// TO TEST:
//	Lookup(*LookupRequest, *LookupResponse)
//	Getattr(*GetattrRequest, *GetattrResponse)
//	Attr with explicit inode
//	Setattr(*SetattrRequest, *SetattrResponse)
//	Access(*AccessRequest)
//	Open(*OpenRequest, *OpenResponse)
//	Write(*WriteRequest, *WriteResponse)
//	Flush(*FlushRequest, *FlushResponse)

func init() {
	fstestutil.DebugByDefault()
}

// symlink can be embedded in a struct to make it look like a symlink.
type symlink struct {
	target string
}

func (f symlink) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeSymlink | 0666
	return nil
}

// fifo can be embedded in a struct to make it look like a named pipe.
type fifo struct{}

func (f fifo) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeNamedPipe | 0666
	return nil
}

func TestMountpointDoesNotExist(t *testing.T) {
	maybeParallel(t)
	tmp, err := ioutil.TempDir("", "fusetest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)

	mountpoint := path.Join(tmp, "does-not-exist")
	conn, err := fuse.Mount(mountpoint)
	if err == nil {
		conn.Close()
		t.Fatalf("expected error with non-existent mountpoint")
	}
	if _, ok := err.(*fuse.MountpointDoesNotExistError); !ok {
		t.Fatalf("wrong error from mount: %T: %v", err, err)
	}
}

type badRootFS struct{}

func (badRootFS) Root() (fs.Node, error) {
	// pick a really distinct error, to identify it later
	return nil, fuse.Errno(syscall.ENAMETOOLONG)
}

func TestRootErr(t *testing.T) {
	maybeParallel(t)
	mnt, err := fstestutil.MountedT(t, badRootFS{}, nil)
	if err == nil {
		// path for synchronous mounts (linux): started out fine, now
		// wait for Serve to cycle through
		err = <-mnt.Error
		// without this, unmount will keep failing with EBUSY; nudge
		// kernel into realizing InitResponse will not happen
		mnt.Conn.Close()
		mnt.Close()
	}

	if err == nil {
		t.Fatal("expected an error")
	}
	// TODO this should not be a textual comparison, Serve hides
	// details
	if err.Error() != "cannot obtain root node: file name too long" {
		t.Errorf("Unexpected error: %v", err)
	}
}

type testPanic struct{}

type panicSentinel struct{}

var _ error = panicSentinel{}

func (panicSentinel) Error() string { return "just a test" }

var _ fuse.ErrorNumber = panicSentinel{}

func (panicSentinel) Errno() fuse.Errno {
	return fuse.Errno(syscall.ENAMETOOLONG)
}

func (f testPanic) Root() (fs.Node, error) {
	return f, nil
}

func (f testPanic) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = 1
	a.Mode = os.ModeDir | 0777
	return nil
}

func (f testPanic) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	panic(panicSentinel{})
}

func doPanic(ctx context.Context, dir string) (*struct{}, error) {
	err := os.Mkdir(dir+"/trigger-a-panic", 0700)
	if nerr, ok := err.(*os.PathError); !ok || nerr.Err != syscall.ENAMETOOLONG {
		return nil, fmt.Errorf("wrong error from panicking handler: %T: %v", err, err)
	}
	return &struct{}{}, nil
}

var panicHelper = helpers.Register("panic", httpjson.ServePOST(doPanic))

func TestPanic(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, testPanic{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := panicHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

type testStatFS struct{}

func (f testStatFS) Root() (fs.Node, error) {
	return f, nil
}

func (f testStatFS) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = 1
	a.Mode = os.ModeDir | 0777
	return nil
}

func (f testStatFS) Statfs(ctx context.Context, req *fuse.StatfsRequest, resp *fuse.StatfsResponse) error {
	resp.Blocks = 42
	resp.Bfree = 10
	resp.Bavail = 3
	resp.Files = 13
	resp.Ffree = 11
	resp.Bsize = 1000
	resp.Namelen = 34
	resp.Frsize = 7
	return nil
}

func doStatfs(ctx context.Context, dir string) (*struct{}, error) {
	// Perform an operation that forces the OS X mount to be ready, so
	// we know the Statfs handler will really be called. OS X insists
	// on volumes answering Statfs calls very early (before FUSE
	// handshake), so OSXFUSE gives made-up answers for a few brief moments
	// during the mount process.
	if _, err := os.Stat(dir + "/does-not-exist"); !os.IsNotExist(err) {
		return nil, err
	}

	success := true
	badf := func(fmt string, args ...interface{}) {
		success = false
		log.Printf(fmt, args...)
	}
	{
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return nil, fmt.Errorf("Statfs failed: %v", err)
		}
		log.Printf("Statfs got: %#v", st)
		if g, e := st.Blocks, uint64(42); g != e {
			badf("got Blocks = %d; want %d", g, e)
		}
		if g, e := st.Bfree, uint64(10); g != e {
			badf("got Bfree = %d; want %d", g, e)
		}
		if g, e := st.Bavail, uint64(3); g != e {
			badf("got Bavail = %d; want %d", g, e)
		}
		if g, e := st.Files, uint64(13); g != e {
			badf("got Files = %d; want %d", g, e)
		}
		if g, e := st.Ffree, uint64(11); g != e {
			badf("got Ffree = %d; want %d", g, e)
		}
		if g, e := st.Bsize, int64(1000); g != e {
			badf("got Bsize = %d; want %d", g, e)
		}
		if g, e := st.Namelen, int64(34); g != e {
			badf("got Namelen = %d; want %d", g, e)
		}
		if g, e := st.Frsize, int64(7); g != e {
			badf("got Frsize = %d; want %d", g, e)
		}
	}

	{
		var st syscall.Statfs_t
		f, err := os.Open(dir)
		if err != nil {
			return nil, fmt.Errorf("Open for fstatfs failed: %v", err)
		}
		defer f.Close()
		err = syscall.Fstatfs(int(f.Fd()), &st)
		if err != nil {
			return nil, fmt.Errorf("Fstatfs failed: %v", err)
		}
		log.Printf("Fstatfs got: %#v", st)
		if g, e := st.Blocks, uint64(42); g != e {
			badf("got Blocks = %d; want %d", g, e)
		}
		if g, e := st.Files, uint64(13); g != e {
			badf("got Files = %d; want %d", g, e)
		}
	}

	if !success {
		return nil, errors.New("bad statfs")
	}
	return &struct{}{}, nil
}

var statfsHelper = helpers.Register("statfs", httpjson.ServePOST(doStatfs))

func TestStatfs(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, testStatFS{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	control := statfsHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

// Test Stat of root.

type root struct{}

func (f root) Root() (fs.Node, error) {
	return f, nil
}

func (root) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = 1
	a.Mode = os.ModeDir | 0555
	// This has to be a power of two, but try to pick something that's an unlikely default.
	a.BlockSize = 65536
	return nil
}

type statResult struct {
	Mode    os.FileMode
	Ino     uint64
	Nlink   uint64
	UID     uint32
	GID     uint32
	Blksize int64
}

func doStat(ctx context.Context, path string) (*statResult, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	r := &statResult{
		Mode: fi.Mode(),
	}
	switch stat := fi.Sys().(type) {
	case *syscall.Stat_t:
		r.Ino = stat.Ino
		r.Nlink = stat.Nlink
		r.UID = stat.Uid
		r.GID = stat.Gid
		// convert stat.Blksize  because it's int64 on Linux but
		// int32 on Darwin.
		r.Blksize = int64(stat.Blksize)
	}
	return r, nil
}

var statHelper = helpers.Register("stat", httpjson.ServePOST(doStat))

func TestStatRoot(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, root{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := statHelper.Spawn(ctx, t)
	defer control.Close()
	var got statResult
	if err := control.JSON("/").Call(ctx, mnt.Dir, &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if (got.Mode & os.ModeType) != os.ModeDir {
		t.Errorf("root is not a directory: %v", got.Mode)
	}
	if p := got.Mode.Perm(); p != 0555 {
		t.Errorf("root has weird access mode: %v", p)
	}
	if got.Ino != 1 {
		t.Errorf("root has wrong inode: %v", got.Ino)
	}
	if got.Nlink != 1 {
		t.Errorf("root has wrong link count: %v", got.Nlink)
	}
	if got.UID != 0 {
		t.Errorf("root has wrong uid: %d", got.UID)
	}
	if got.GID != 0 {
		t.Errorf("root has wrong gid: %d", got.GID)
	}
	if mnt.Conn.Protocol().HasAttrBlockSize() {
		// convert got.Blksize too because it's int64 on Linux but
		// int32 on Darwin.
		if g, e := int64(got.Blksize), int64(65536); g != e {
			t.Errorf("root has wrong blocksize: %d != %d", g, e)
		}
	}
}

// Test Read calling ReadAll.

type readAll struct {
	fstestutil.File
}

const hi = "hello, world"

func (readAll) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0666
	a.Size = uint64(len(hi))
	return nil
}

func (readAll) ReadAll(ctx context.Context) ([]byte, error) {
	return []byte(hi), nil
}

type readResult struct {
	Data []byte
}

func doRead(ctx context.Context, path string) (*readResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, 4096)
	n, err := f.Read(data)
	if err != nil {
		return nil, err
	}
	r := &readResult{Data: data[:n]}
	return r, nil
}

var readHelper = helpers.Register("read", httpjson.ServePOST(doRead))

func TestReadAll(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": readAll{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readHelper.Spawn(ctx, t)
	defer control.Close()
	var got readResult
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := string(got.Data), hi; g != e {
		t.Errorf("readAll = %q, want %q", g, e)
	}
}

// Test Read.

type readWithHandleRead struct {
	fstestutil.File
}

func (readWithHandleRead) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0666
	a.Size = uint64(len(hi))
	return nil
}

func (readWithHandleRead) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	fuseutil.HandleRead(req, resp, []byte(hi))
	return nil
}

func TestReadAllWithHandleRead(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": readWithHandleRead{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readHelper.Spawn(ctx, t)
	defer control.Close()
	var got readResult
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := string(got.Data), hi; g != e {
		t.Errorf("readAll = %q, want %q", g, e)
	}
}

type readFlags struct {
	fstestutil.File
	fileFlags record.Recorder
}

func (r *readFlags) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0666
	a.Size = uint64(len(hi))
	return nil
}

func (r *readFlags) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	r.fileFlags.Record(req.FileFlags)
	fuseutil.HandleRead(req, resp, []byte(hi))
	return nil
}

func doReadFileFlags(ctx context.Context, path string) (*struct{}, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Read(make([]byte, 4096)); err != nil {
		return nil, err
	}
	_ = f.Close()
	return &struct{}{}, nil
}

var readFileFlagsHelper = helpers.Register("readFileFlags", httpjson.ServePOST(doReadFileFlags))

func TestReadFileFlags(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &readFlags{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": r}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	if !mnt.Conn.Protocol().HasReadWriteFlags() {
		t.Skip("Old FUSE protocol")
	}

	control := readFileFlagsHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	got := r.fileFlags.Recorded().(fuse.OpenFlags)
	got &^= fuse.OpenNonblock
	want := fuse.OpenReadWrite | fuse.OpenAppend
	if runtime.GOOS == "darwin" {
		// OSXFUSE shares one read and one write handle for all
		// clients, so it uses a OpenReadOnly handle for performing
		// our read.
		//
		// If this test starts failing in the future, that probably
		// means they added the feature, and we want to notice that!
		want = fuse.OpenReadOnly
	}
	if g, e := got, want; g != e {
		t.Errorf("read saw file flags %+v, want %+v", g, e)
	}
}

type writeFlags struct {
	fstestutil.File
	fileFlags record.Recorder
}

func (r *writeFlags) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0666
	a.Size = uint64(len(hi))
	return nil
}

func (r *writeFlags) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	// OSXFUSE 3.0.4 does a read-modify-write cycle even when the
	// write was for 4096 bytes.
	fuseutil.HandleRead(req, resp, []byte(hi))
	return nil
}

func (r *writeFlags) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	r.fileFlags.Record(req.FileFlags)
	resp.Size = len(req.Data)
	return nil
}

func doWriteFileFlags(ctx context.Context, path string) (*struct{}, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Write(make([]byte, 4096)); err != nil {
		return nil, err
	}
	_ = f.Close()
	return &struct{}{}, nil
}

var writeFileFlagsHelper = helpers.Register("writeFileFlags", httpjson.ServePOST(doWriteFileFlags))

func TestWriteFileFlags(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &writeFlags{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": r}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	if !mnt.Conn.Protocol().HasReadWriteFlags() {
		t.Skip("Old FUSE protocol")
	}

	control := writeFileFlagsHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	got := r.fileFlags.Recorded().(fuse.OpenFlags)
	got &^= fuse.OpenNonblock
	want := fuse.OpenReadWrite | fuse.OpenAppend
	if runtime.GOOS == "darwin" {
		// OSXFUSE shares one read and one write handle for all
		// clients, so it uses a OpenWriteOnly handle for performing
		// our read.
		//
		// If this test starts failing in the future, that probably
		// means they added the feature, and we want to notice that!
		want = fuse.OpenWriteOnly
	}
	if g, e := got, want; g != e {
		t.Errorf("write saw file flags %+v, want %+v", g, e)
	}
}

// Test Release.

type release struct {
	fstestutil.File
	record.ReleaseWaiter
}

func doOpen(ctx context.Context, path string) (*struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &struct{}{}, nil
}

var openHelper = helpers.Register("open", httpjson.ServePOST(doOpen))

func TestRelease(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &release{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": r}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	control := openHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if !r.WaitForRelease(1 * time.Second) {
		t.Error("Close did not Release in time")
	}
}

// Test Write calling basic Write, with an fsync thrown in too.

type write struct {
	fstestutil.File
	record.Writes
	record.Fsyncs
}

type createWriteFsyncHelp struct {
	mu   sync.Mutex
	file *os.File
}

func (cwf *createWriteFsyncHelp) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/createWrite":
		httpjson.ServePOST(cwf.doCreateWrite).ServeHTTP(w, req)
	case "/fsync":
		httpjson.ServePOST(cwf.doFsync).ServeHTTP(w, req)
	case "/close":
		httpjson.ServePOST(cwf.doClose).ServeHTTP(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (cwf *createWriteFsyncHelp) doCreateWrite(ctx context.Context, path string) (*struct{}, error) {
	cwf.mu.Lock()
	defer cwf.mu.Unlock()
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("Create: %v", err)
	}
	cwf.file = f
	n, err := f.Write([]byte(hi))
	if err != nil {
		return nil, fmt.Errorf("Write: %v", err)
	}
	if n != len(hi) {
		return nil, fmt.Errorf("short write; n=%d; hi=%d", n, len(hi))
	}
	return &struct{}{}, nil
}

func (cwf *createWriteFsyncHelp) doFsync(ctx context.Context, _ struct{}) (*struct{}, error) {
	cwf.mu.Lock()
	defer cwf.mu.Unlock()
	if err := cwf.file.Sync(); err != nil {
		return nil, fmt.Errorf("Fsync = %v", err)
	}
	return &struct{}{}, nil
}

func (cwf *createWriteFsyncHelp) doClose(ctx context.Context, _ struct{}) (*struct{}, error) {
	cwf.mu.Lock()
	defer cwf.mu.Unlock()
	if err := cwf.file.Close(); err != nil {
		return nil, fmt.Errorf("Close: %v", err)
	}

	return &struct{}{}, nil
}

var createWriteFsyncHelper = helpers.Register("createWriteFsync", &createWriteFsyncHelp{})

func TestWrite(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &write{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": w}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	control := createWriteFsyncHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/createWrite").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if err := control.JSON("/fsync").Call(ctx, struct{}{}, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if w.RecordedFsync() == (fuse.FsyncRequest{}) {
		t.Errorf("never received expected fsync call")
	}
	if got := string(w.RecordedWriteData()); got != hi {
		t.Errorf("write = %q, want %q", got, hi)
	}
	if err := control.JSON("/close").Call(ctx, struct{}{}, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

// Test Write of a larger buffer.

func makeLargeData() (one, large []byte) {
	o := []byte("xyzzyfoo")
	l := bytes.Repeat(o, 8192)
	return o, l
}

func doWriteLarge(ctx context.Context, path string) (*struct{}, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("Create: %v", err)
	}
	defer f.Close()
	_, large := makeLargeData()
	n, err := f.Write(large)
	if err != nil {
		return nil, fmt.Errorf("Write: %v", err)
	}
	if g, e := n, len(large); g != e {
		return nil, fmt.Errorf("short write: %d != %d", g, e)
	}
	err = f.Close()
	if err != nil {
		return nil, fmt.Errorf("Close: %v", err)
	}
	return &struct{}{}, nil
}

var writeLargeHelper = helpers.Register("writeLarge", httpjson.ServePOST(doWriteLarge))

func TestWriteLarge(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &write{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": w}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	control := writeLargeHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	got := w.RecordedWriteData()
	one, large := makeLargeData()
	if g, e := len(got), len(large); g != e {
		t.Errorf("write wrong length: %d != %d", g, e)
	}
	if g := bytes.Replace(got, one, nil, -1); len(g) > 0 {
		t.Errorf("write wrong data: expected repeats of %q, also got %q", one, g)
	}
}

// Test Write calling Setattr+Write+Flush.

type writeTruncateFlush struct {
	fstestutil.File
	record.Writes
	record.Setattrs
	record.Flushes
}

type writeFileRequest struct {
	Path string
	Data []byte
}

func doWriteFile(ctx context.Context, req writeFileRequest) (*struct{}, error) {
	if err := ioutil.WriteFile(req.Path, req.Data, 0666); err != nil {
		return nil, fmt.Errorf("WriteFile: %v", err)
	}
	return &struct{}{}, nil
}

var writeFileHelper = helpers.Register("writeFile", httpjson.ServePOST(doWriteFile))

func TestWriteTruncateFlush(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &writeTruncateFlush{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": w}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	control := writeFileHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	req := writeFileRequest{
		Path: mnt.Dir + "/child",
		Data: []byte(hi),
	}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if w.RecordedSetattr() == (fuse.SetattrRequest{}) {
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
	fstestutil.Dir
	record.Mkdirs
}

func (f *mkdir1) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	f.Mkdirs.Mkdir(ctx, req)
	return &mkdir1{}, nil
}

func doMkdir(ctx context.Context, path string) (*struct{}, error) {
	// uniform umask needed to make os.Mkdir's mode into something
	// reproducible
	syscall.Umask(0022)
	if err := os.Mkdir(path, 0771); err != nil {
		return nil, fmt.Errorf("mkdir: %v", err)
	}
	return &struct{}{}, nil
}

var mkdirHelper = helpers.Register("mkdir", httpjson.ServePOST(doMkdir))

func TestMkdir(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &mkdir1{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	control := mkdirHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/foo", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	want := fuse.MkdirRequest{Name: "foo", Mode: os.ModeDir | 0751}
	if mnt.Conn.Protocol().HasUmask() {
		want.Umask = 0022
	}
	if runtime.GOOS == "darwin" {
		// https://github.com/osxfuse/osxfuse/issues/225
		want.Umask = 0
	}
	if g, e := f.RecordedMkdir(), want; g != e {
		t.Errorf("mkdir saw %+v, want %+v", g, e)
	}
}

// Test Create

type create1file struct {
	fstestutil.File
	record.Creates
}

type create1 struct {
	fstestutil.Dir
	f create1file
}

func (f *create1) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	if req.Name != "foo" {
		log.Printf("ERROR create1.Create unexpected name: %q\n", req.Name)
		return nil, nil, fuse.EPERM
	}

	_, _, _ = f.f.Creates.Create(ctx, req, resp)
	return &f.f, &f.f, nil
}

func doCreate(ctx context.Context, path string) (*struct{}, error) {
	// uniform umask needed to make os.Mkdir's mode into something
	// reproducible
	syscall.Umask(0022)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0640)
	if err != nil {
		return nil, fmt.Errorf("create1 WriteFile: %v", err)
	}
	_ = f.Close()
	return &struct{}{}, nil
}

var createHelper = helpers.Register("create", httpjson.ServePOST(doCreate))

func TestCreate(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &create1{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	control := createHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/foo", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	want := fuse.CreateRequest{
		Name:  "foo",
		Flags: fuse.OpenReadWrite | fuse.OpenCreate | fuse.OpenTruncate,
		Mode:  0640,
	}
	if mnt.Conn.Protocol().HasUmask() {
		want.Umask = 0022
	}
	if runtime.GOOS == "darwin" {
		// OS X does not pass O_TRUNC here, Linux does; as this is a
		// Create, that's acceptable
		want.Flags &^= fuse.OpenTruncate

		// https://github.com/osxfuse/osxfuse/issues/225
		want.Umask = 0
	}
	got := f.f.RecordedCreate()
	if runtime.GOOS == "linux" {
		// Linux <3.7 accidentally leaks O_CLOEXEC through to FUSE;
		// avoid spurious test failures
		got.Flags &^= fuse.OpenFlags(syscall.O_CLOEXEC)
	}
	if g, e := got, want; g != e {
		t.Fatalf("create saw %+v, want %+v", g, e)
	}
}

// Test Create + Write + Remove

type create3file struct {
	fstestutil.File
	record.Writes
}

type create3 struct {
	fstestutil.Dir
	f          create3file
	fooCreated record.MarkRecorder
	fooRemoved record.MarkRecorder
}

func (f *create3) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	if req.Name != "foo" {
		log.Printf("ERROR create3.Create unexpected name: %q\n", req.Name)
		return nil, nil, fuse.EPERM
	}
	f.fooCreated.Mark()
	return &f.f, &f.f, nil
}

func (f *create3) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if f.fooCreated.Recorded() && !f.fooRemoved.Recorded() && name == "foo" {
		return &f.f, nil
	}
	return nil, fuse.ENOENT
}

func (f *create3) Remove(ctx context.Context, r *fuse.RemoveRequest) error {
	if f.fooCreated.Recorded() && !f.fooRemoved.Recorded() &&
		r.Name == "foo" && !r.Dir {
		f.fooRemoved.Mark()
		return nil
	}
	return fuse.ENOENT
}

func doCreateWriteRemove(ctx context.Context, path string) (*struct{}, error) {
	if err := ioutil.WriteFile(path, []byte(hi), 0666); err != nil {
		return nil, fmt.Errorf("WriteFile: %v", err)
	}
	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("Remove: %v", err)
	}
	if err := os.Remove(path); !errors.Is(err, syscall.ENOENT) {
		return nil, fmt.Errorf("second Remove: wrong error: %v", err)
	}
	return &struct{}{}, nil
}

var createWriteRemoveHelper = helpers.Register("createWriteRemove", httpjson.ServePOST(doCreateWriteRemove))

func TestCreateWriteRemove(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &create3{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := createWriteRemoveHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/foo", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

// Test symlink + readlink

// is a Node that is a symlink to target
type symlink1link struct {
	symlink
	target string
}

func (f symlink1link) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (string, error) {
	return f.target, nil
}

type symlink1 struct {
	fstestutil.Dir
	record.Symlinks
}

func (f *symlink1) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fs.Node, error) {
	f.Symlinks.Symlink(ctx, req)
	return symlink1link{target: req.Target}, nil
}

type symlinkHelp struct{}

func (i *symlinkHelp) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/symlink":
		httpjson.ServePOST(i.doSymlink).ServeHTTP(w, req)
	case "/readlink":
		httpjson.ServePOST(i.doReadlink).ServeHTTP(w, req)
	default:
		http.NotFound(w, req)
	}
}

type symlinkRequest struct {
	Target string
	Path   string
}

func (i *symlinkHelp) doSymlink(ctx context.Context, req symlinkRequest) (*struct{}, error) {
	if err := os.Symlink(req.Target, req.Path); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

func (i *symlinkHelp) doReadlink(ctx context.Context, path string) (string, error) {
	return os.Readlink(path)
}

var symlinkHelper = helpers.Register("symlink", &symlinkHelp{})

func TestSymlink(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &symlink1{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := symlinkHelper.Spawn(ctx, t)
	defer control.Close()

	const target = "/some-target"
	path := mnt.Dir + "/symlink.file"
	req := symlinkRequest{
		Target: target,
		Path:   path,
	}
	var nothing struct{}
	if err := control.JSON("/symlink").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	want := fuse.SymlinkRequest{NewName: "symlink.file", Target: target}
	if g, e := f.RecordedSymlink(), want; g != e {
		t.Errorf("symlink saw %+v, want %+v", g, e)
	}

	var gotName string
	if err := control.JSON("/readlink").Call(ctx, path, &gotName); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if gotName != target {
		t.Errorf("os.Readlink = %q; want %q", gotName, target)
	}
}

// Test link

type link1 struct {
	fstestutil.Dir
	record.Links
}

func (f *link1) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if name == "old" {
		return fstestutil.File{}, nil
	}
	return nil, fuse.ENOENT
}

func (f *link1) Link(ctx context.Context, r *fuse.LinkRequest, old fs.Node) (fs.Node, error) {
	f.Links.Link(ctx, r, old)
	return fstestutil.File{}, nil
}

type linkRequest struct {
	OldName string
	NewName string
}

func doLink(ctx context.Context, req linkRequest) (*struct{}, error) {
	if err := os.Link(req.OldName, req.NewName); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

var linkHelper = helpers.Register("link", httpjson.ServePOST(doLink))

func TestLink(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &link1{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := linkHelper.Spawn(ctx, t)
	defer control.Close()

	req := linkRequest{
		OldName: mnt.Dir + "/old",
		NewName: mnt.Dir + "/new",
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
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

// Test Rename

type rename1 struct {
	fstestutil.Dir
	renamed record.Counter
}

func (f *rename1) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if name == "old" {
		return fstestutil.File{}, nil
	}
	return nil, fuse.ENOENT
}

func (f *rename1) Rename(ctx context.Context, r *fuse.RenameRequest, newDir fs.Node) error {
	if r.OldName == "old" && r.NewName == "new" && newDir == f {
		f.renamed.Inc()
		return nil
	}
	return fuse.EIO
}

type renameRequest struct {
	OldName   string
	NewName   string
	WantErrno syscall.Errno
}

func doRename(ctx context.Context, req renameRequest) (*struct{}, error) {
	if err := os.Rename(req.OldName, req.NewName); !errors.Is(err, req.WantErrno) {
		return nil, err
	}
	return &struct{}{}, nil
}

var renameHelper = helpers.Register("rename", httpjson.ServePOST(doRename))

func TestRename(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &rename1{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := renameHelper.Spawn(ctx, t)
	defer control.Close()

	{
		req := renameRequest{
			OldName: mnt.Dir + "/old",
			NewName: mnt.Dir + "/new",
		}
		var nothing struct{}
		if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
			t.Fatalf("calling helper: %v", err)
		}
	}
	if g, e := f.renamed.Count(), uint32(1); g != e {
		t.Fatalf("expected rename didn't happen: %d != %d", g, e)
	}
	{
		req := renameRequest{
			OldName:   mnt.Dir + "/old2",
			NewName:   mnt.Dir + "/new2",
			WantErrno: syscall.ENOENT,
		}
		var nothing struct{}
		if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
			t.Fatalf("calling helper: %v", err)
		}
	}
}

// Test mknod

type mknod1 struct {
	fstestutil.Dir
	record.Mknods
}

func (f *mknod1) Mknod(ctx context.Context, r *fuse.MknodRequest) (fs.Node, error) {
	f.Mknods.Mknod(ctx, r)
	return fifo{}, nil
}

func doMknod(ctx context.Context, path string) (*struct{}, error) {
	// uniform umask needed to make mknod's mode into something
	// reproducible
	syscall.Umask(0022)
	if err := syscall.Mknod(path, syscall.S_IFIFO|0660, 123); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

var mknodHelper = helpers.Register("mknod", httpjson.ServePOST(doMknod))

func TestMknod(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("skipping unless root")
	}
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := &mknod1{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := mknodHelper.Spawn(ctx, t)
	defer control.Close()

	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/node", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	want := fuse.MknodRequest{
		Name: "node",
		Mode: os.FileMode(os.ModeNamedPipe | 0640),
		Rdev: uint32(123),
	}
	if runtime.GOOS == "linux" {
		// Linux fuse doesn't echo back the rdev if the node
		// isn't a device (we're using a FIFO here, as that
		// bit is portable.)
		want.Rdev = 0
	}
	if mnt.Conn.Protocol().HasUmask() {
		want.Umask = 0022
	}
	if runtime.GOOS == "darwin" {
		// https://github.com/osxfuse/osxfuse/issues/225
		want.Umask = 0
	}
	if g, e := f.RecordedMknod(), want; g != e {
		t.Fatalf("mknod saw %+v, want %+v", g, e)
	}
}

// Test Read served with DataHandle.

type dataHandleTest struct {
	fstestutil.File
}

func (dataHandleTest) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0666
	a.Size = uint64(len(hi))
	return nil
}

func (dataHandleTest) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	return fs.DataHandle([]byte(hi)), nil
}

func TestDataHandle(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &dataHandleTest{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readHelper.Spawn(ctx, t)
	defer control.Close()

	var got readResult
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := string(got.Data), hi; g != e {
		t.Errorf("readAll = %q, want %q", g, e)
	}
}

// Test interrupt

type interrupt struct {
	fstestutil.File

	// strobes to signal we have a read hanging
	hanging chan struct{}
	// strobes to signal kernel asked us to interrupt read
	interrupted chan struct{}
}

func (interrupt) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0666
	a.Size = 1
	return nil
}

func (it *interrupt) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	select {
	case it.hanging <- struct{}{}:
	default:
	}
	log.Printf("reading...")
	<-ctx.Done()
	log.Printf("read done")
	select {
	case it.interrupted <- struct{}{}:
	default:
	}
	return ctx.Err()
}

type interruptHelp struct {
	mu     sync.Mutex
	result *interruptResult
}

type interruptResult struct {
	OK    bool
	Read  []byte
	Error string
}

func (i *interruptHelp) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/read":
		httpjson.ServePOST(i.doRead).ServeHTTP(w, req)
	case "/report":
		httpjson.ServePOST(i.doReport).ServeHTTP(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (i *interruptHelp) doRead(ctx context.Context, dir string) (struct{}, error) {
	log.SetPrefix("interrupt child: ")
	log.SetFlags(0)

	log.Printf("starting...")

	f, err := os.Open(filepath.Join(dir, "child"))
	if err != nil {
		log.Fatalf("cannot open file: %v", err)
	}

	i.mu.Lock()
	// background this so we can return a response to the test
	go func() {
		defer i.mu.Unlock()
		defer f.Close()
		log.Printf("reading...")
		buf := make([]byte, 4096)
		n, err := syscall.Read(int(f.Fd()), buf)
		var r interruptResult
		switch err {
		case nil:
			buf = buf[:n]
			log.Printf("read: expected error, got data: %q", buf)
			r.Read = buf
		case syscall.EINTR:
			log.Printf("read: saw EINTR, all good")
			r.OK = true
		default:
			msg := err.Error()
			log.Printf("read: wrong error: %s", msg)
			r.Error = msg
		}
		i.result = &r
		log.Printf("read done...")
	}()
	return struct{}{}, nil
}

func (i *interruptHelp) doReport(ctx context.Context, _ struct{}) (*interruptResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.result == nil {
		return nil, errors.New("no result yet")
	}
	return i.result, nil
}

var interruptHelper = helpers.Register("interrupt", &interruptHelp{})

func TestInterrupt(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := &interrupt{
		hanging:     make(chan struct{}, 1),
		interrupted: make(chan struct{}, 1),
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	// start a subprocess that can hang until signaled
	control := interruptHelper.Spawn(ctx, t)
	defer control.Close()

	var nothing struct{}
	if err := control.JSON("/read").Call(ctx, mnt.Dir, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	// wait till we're sure it's hanging in read
	<-f.hanging

	if err := control.Signal(syscall.SIGSTOP); err != nil {
		t.Errorf("cannot send SIGSTOP: %v", err)
		return
	}

	// give the process enough time to receive SIGSTOP, otherwise it
	// won't interrupt the syscall.
	<-f.interrupted

	if err := control.Signal(syscall.SIGCONT); err != nil {
		t.Errorf("cannot send SIGCONT: %v", err)
		return
	}

	var result interruptResult
	if err := control.JSON("/report").Call(ctx, struct{}{}, &result); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if !result.OK {
		if msg := result.Error; msg != "" {
			t.Errorf("unexpected error from read: %v", msg)
		}
		if data := result.Read; len(data) > 0 {
			t.Errorf("unexpected successful read: %q", data)
		}
	}
}

// Test deadline

type deadline struct {
	fstestutil.File
}

var _ fs.NodeOpener = (*deadline)(nil)

func (it *deadline) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type openRequest struct {
	Path      string
	WantErrno syscall.Errno
}

func doOpenErr(ctx context.Context, req openRequest) (*struct{}, error) {
	f, err := os.Open(req.Path)
	if err == nil {
		f.Close()
	}
	if !errors.Is(err, req.WantErrno) {
		return nil, err
	}
	return &struct{}{}, nil
}

var openErrHelper = helpers.Register("openErr", httpjson.ServePOST(doOpenErr))

func TestDeadline(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child := &deadline{}
	config := &fs.Config{
		WithContext: func(ctx context.Context, req fuse.Request) context.Context {
			// return a context that has already deadlined

			// Server.serve will cancel the parent context, which will
			// cancel this one, so discarding cancel here should be
			// safe.
			ctx, _ = context.WithDeadline(ctx, time.Unix(0, 0))
			return ctx
		},
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": child}}, config)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := openErrHelper.Spawn(ctx, t)
	defer control.Close()

	req := openRequest{
		Path: mnt.Dir + "/child",
		// not caused by signal -> should not get EINTR;
		// context.DeadlineExceeded will be translated into EIO
		WantErrno: syscall.EIO,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

// Test truncate

type truncate struct {
	fstestutil.File
	record.Setattrs
}

type truncateRequest struct {
	Path   string
	ToSize int64
}

func doTruncate(ctx context.Context, req truncateRequest) (*struct{}, error) {
	if err := os.Truncate(req.Path, req.ToSize); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

var truncateHelper = helpers.Register("truncate", httpjson.ServePOST(doTruncate))

func testTruncate(t *testing.T, toSize int64) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &truncate{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := truncateHelper.Spawn(ctx, t)
	defer control.Close()

	req := truncateRequest{
		Path:   mnt.Dir + "/child",
		ToSize: toSize,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	gotr := f.RecordedSetattr()
	if gotr == (fuse.SetattrRequest{}) {
		t.Fatalf("no recorded SetattrRequest")
	}
	if g, e := gotr.Size, uint64(toSize); g != e {
		t.Errorf("got Size = %q; want %q", g, e)
	}
	if g, e := gotr.Valid&^fuse.SetattrLockOwner, fuse.SetattrSize; g != e {
		t.Errorf("got Valid = %q; want %q", g, e)
	}
	t.Logf("Got request: %#v", gotr)
}

func TestTruncate(t *testing.T) {
	t.Run("42", func(t *testing.T) { testTruncate(t, 42) })
	t.Run("0", func(t *testing.T) { testTruncate(t, 0) })
}

// Test ftruncate

type ftruncate struct {
	fstestutil.File
	record.Setattrs
}

func doFtruncate(ctx context.Context, req truncateRequest) (*struct{}, error) {
	f, err := os.OpenFile(req.Path, os.O_WRONLY, 0666)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := f.Truncate(req.ToSize); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

var ftruncateHelper = helpers.Register("ftruncate", httpjson.ServePOST(doFtruncate))

func testFtruncate(t *testing.T, toSize int64) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &ftruncate{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := ftruncateHelper.Spawn(ctx, t)
	defer control.Close()

	req := truncateRequest{
		Path:   mnt.Dir + "/child",
		ToSize: toSize,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	gotr := f.RecordedSetattr()
	if gotr == (fuse.SetattrRequest{}) {
		t.Fatalf("no recorded SetattrRequest")
	}
	if g, e := gotr.Size, uint64(toSize); g != e {
		t.Errorf("got Size = %q; want %q", g, e)
	}
	if g, e := gotr.Valid&^fuse.SetattrLockOwner, fuse.SetattrHandle|fuse.SetattrSize; g != e {
		t.Errorf("got Valid = %q; want %q", g, e)
	}
	t.Logf("Got request: %#v", gotr)
}

func TestFtruncate(t *testing.T) {
	t.Run("42", func(t *testing.T) { testFtruncate(t, 42) })
	t.Run("0", func(t *testing.T) { testFtruncate(t, 0) })
}

// Test opening existing file truncates

type truncateWithOpen struct {
	fstestutil.File
	record.Setattrs
}

func TestTruncateWithOpen(t *testing.T) {
	maybeParallel(t)
	f := &truncateWithOpen{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	fil, err := os.OpenFile(mnt.Dir+"/child", os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		t.Error(err)
		return
	}
	fil.Close()

	gotr := f.RecordedSetattr()
	if gotr == (fuse.SetattrRequest{}) {
		t.Fatalf("no recorded SetattrRequest")
	}
	if g, e := gotr.Size, uint64(0); g != e {
		t.Errorf("got Size = %q; want %q", g, e)
	}
	// osxfuse sets SetattrHandle here, linux does not
	if g, e := gotr.Valid&^(fuse.SetattrLockOwner|fuse.SetattrHandle), fuse.SetattrSize; g != e {
		t.Errorf("got Valid = %q; want %q", g, e)
	}
	t.Logf("Got request: %#v", gotr)
}

// Test readdir calling ReadDirAll

type readDirAll struct {
	fstestutil.Dir
}

func (d *readDirAll) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return []fuse.Dirent{
		{Name: "one", Inode: 11, Type: fuse.DT_Dir},
		{Name: "three", Inode: 13},
		{Name: "two", Inode: 12, Type: fuse.DT_File},
	}, nil
}

func TestReadDirAll(t *testing.T) {
	maybeParallel(t)
	f := &readDirAll{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	fil, err := os.Open(mnt.Dir)
	if err != nil {
		t.Error(err)
		return
	}
	defer fil.Close()

	// go Readdir is just Readdirnames + Lstat, there's no point in
	// testing that here; we have no consumption API for the real
	// dirent data
	names, err := fil.Readdirnames(100)
	if err != nil {
		t.Error(err)
		return
	}

	t.Logf("Got readdir: %q", names)

	if len(names) != 3 ||
		names[0] != "one" ||
		names[1] != "three" ||
		names[2] != "two" {
		t.Errorf(`expected 3 entries of "one", "three", "two", got: %q`, names)
		return
	}
}

type readDirAllBad struct {
	fstestutil.Dir
}

func (d *readDirAllBad) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	r := []fuse.Dirent{
		{Name: "one", Inode: 11, Type: fuse.DT_Dir},
		{Name: "three", Inode: 13},
		{Name: "two", Inode: 12, Type: fuse.DT_File},
	}
	// pick a really distinct error, to identify it later
	return r, fuse.Errno(syscall.ENAMETOOLONG)
}

func TestReadDirAllBad(t *testing.T) {
	maybeParallel(t)
	f := &readDirAllBad{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	fil, err := os.Open(mnt.Dir)
	if err != nil {
		t.Error(err)
		return
	}
	defer fil.Close()

	var names []string
	for {
		n, err := fil.Readdirnames(1)
		if err != nil {
			if nerr, ok := err.(*os.SyscallError); !ok || nerr.Err != syscall.ENAMETOOLONG {
				t.Fatalf("wrong error: %v", err)
			}
			break
		}
		names = append(names, n...)
	}

	t.Logf("Got readdir: %q", names)

	// TODO could serve partial results from ReadDirAll but the
	// shandle.readData mechanism makes that awkward.
	if len(names) != 0 {
		t.Errorf(`expected 0 entries, got: %q`, names)
		return
	}
}

// Test readdir without any ReadDir methods implemented.

type readDirNotImplemented struct {
	fstestutil.Dir
}

func TestReadDirNotImplemented(t *testing.T) {
	maybeParallel(t)
	f := &readDirNotImplemented{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	fil, err := os.Open(mnt.Dir)
	if err != nil {
		t.Error(err)
		return
	}
	defer fil.Close()

	// go Readdir is just Readdirnames + Lstat, there's no point in
	// testing that here; we have no consumption API for the real
	// dirent data
	names, err := fil.Readdirnames(100)
	if len(names) > 0 || err != io.EOF {
		t.Fatalf("expected EOF got names=%v err=%v", names, err)
	}
}

type readDirAllRewind struct {
	fstestutil.Dir
	entries atomic.Value
}

func (d *readDirAllRewind) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	entries := d.entries.Load().([]fuse.Dirent)
	return entries, nil
}

func TestReadDirAllRewind(t *testing.T) {
	maybeParallel(t)
	f := &readDirAllRewind{}
	f.entries.Store([]fuse.Dirent{
		{Name: "one", Inode: 11, Type: fuse.DT_Dir},
	})
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	fil, err := os.Open(mnt.Dir)
	if err != nil {
		t.Error(err)
		return
	}
	defer fil.Close()

	{
		names, err := fil.Readdirnames(100)
		if err != nil {
			t.Error(err)
			return
		}
		t.Logf("Got readdir: %q", names)
		if len(names) != 1 ||
			names[0] != "one" {
			t.Errorf(`expected  entry of "one", got: %q`, names)
			return
		}
	}

	f.entries.Store([]fuse.Dirent{
		{Name: "two", Inode: 12, Type: fuse.DT_File},
		{Name: "one", Inode: 11, Type: fuse.DT_Dir},
	})
	if _, err := fil.Seek(0, os.SEEK_SET); err != nil {
		t.Fatal(err)
	}

	{
		names, err := fil.Readdirnames(100)
		if err != nil {
			t.Error(err)
			return
		}
		t.Logf("Got readdir: %q", names)
		if len(names) != 2 ||
			names[0] != "two" ||
			names[1] != "one" {
			t.Errorf(`expected 2 entries of "two", "one", got: %q`, names)
			return
		}
	}
}

// Test Chmod.

type chmod struct {
	fstestutil.File
	record.Setattrs
}

func (f *chmod) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	if !req.Valid.Mode() {
		log.Printf("setattr not a chmod: %v", req.Valid)
		return fuse.EIO
	}
	f.Setattrs.Setattr(ctx, req, resp)
	return nil
}

func TestChmod(t *testing.T) {
	maybeParallel(t)
	f := &chmod{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	err = os.Chmod(mnt.Dir+"/child", 0764)
	if err != nil {
		t.Errorf("chmod: %v", err)
		return
	}
	got := f.RecordedSetattr()
	if g, e := got.Mode, os.FileMode(0764); g != e {
		t.Errorf("wrong mode: %v != %v", g, e)
	}
}

// Test open

type open struct {
	fstestutil.File
	record.Opens
}

func (f *open) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	f.Opens.Open(ctx, req, resp)
	// pick a really distinct error, to identify it later
	return nil, fuse.Errno(syscall.ENAMETOOLONG)
}

func TestOpen(t *testing.T) {
	maybeParallel(t)
	f := &open{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	// node: mode only matters with O_CREATE
	fil, err := os.OpenFile(mnt.Dir+"/child", os.O_WRONLY|os.O_APPEND, 0)
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

	want := fuse.OpenRequest{Dir: false, Flags: fuse.OpenWriteOnly | fuse.OpenAppend}
	if runtime.GOOS == "darwin" {
		// osxfuse does not let O_APPEND through at all
		//
		// https://code.google.com/p/macfuse/issues/detail?id=233
		// https://code.google.com/p/macfuse/issues/detail?id=132
		// https://code.google.com/p/macfuse/issues/detail?id=133
		want.Flags &^= fuse.OpenAppend
	}
	got := f.RecordedOpen()

	if runtime.GOOS == "linux" {
		// Linux <3.7 accidentally leaks O_CLOEXEC through to FUSE;
		// avoid spurious test failures
		got.Flags &^= fuse.OpenFlags(syscall.O_CLOEXEC)
	}

	if g, e := got, want; g != e {
		t.Errorf("open saw %v, want %v", g, e)
		return
	}
}

type openNonSeekable struct {
	fstestutil.File
}

func (f *openNonSeekable) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	resp.Flags |= fuse.OpenNonSeekable
	return f, nil
}

func TestOpenNonSeekable(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("OSXFUSE shares one read and one write handle for all clients, does not support open modes")
	}

	maybeParallel(t)
	f := &openNonSeekable{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	if !mnt.Conn.Protocol().HasOpenNonSeekable() {
		t.Skip("Old FUSE protocol")
	}

	fil, err := os.Open(mnt.Dir + "/child")
	if err != nil {
		t.Fatal(err)
	}
	defer fil.Close()

	_, err = fil.Seek(0, os.SEEK_SET)
	if nerr, ok := err.(*os.PathError); !ok || nerr.Err != syscall.ESPIPE {
		t.Fatalf("wrong error: %v", err)
	}
}

// Test Fsync on a dir

type fsyncDir struct {
	fstestutil.Dir
	record.Fsyncs
}

func TestFsyncDir(t *testing.T) {
	maybeParallel(t)
	f := &fsyncDir{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	fil, err := os.Open(mnt.Dir)
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

	got := f.RecordedFsync()
	want := fuse.FsyncRequest{
		Flags: 0,
		Dir:   true,
		// unpredictable
		Handle: got.Handle,
	}
	if runtime.GOOS == "darwin" {
		// TODO document the meaning of these flags, figure out why
		// they differ
		want.Flags = 1
	}
	if g, e := got, want; g != e {
		t.Fatalf("fsyncDir saw %+v, want %+v", g, e)
	}
}

// Test Getxattr

type getxattr struct {
	fstestutil.File
	record.Getxattrs
}

func (f *getxattr) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	f.Getxattrs.Getxattr(ctx, req, resp)
	resp.Xattr = []byte("hello, world")
	return nil
}

func TestGetxattr(t *testing.T) {
	maybeParallel(t)
	f := &getxattr{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	buf := make([]byte, 8192)
	n, err := syscallx.Getxattr(mnt.Dir+"/child", "not-there", buf)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	buf = buf[:n]
	if g, e := string(buf), "hello, world"; g != e {
		t.Errorf("wrong getxattr content: %#v != %#v", g, e)
	}
	seen := f.RecordedGetxattr()
	if g, e := seen.Name, "not-there"; g != e {
		t.Errorf("wrong getxattr name: %#v != %#v", g, e)
	}
}

// Test Getxattr that has no space to return value

type getxattrTooSmall struct {
	fstestutil.File
}

func (f *getxattrTooSmall) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	resp.Xattr = []byte("hello, world")
	return nil
}

func TestGetxattrTooSmall(t *testing.T) {
	maybeParallel(t)
	f := &getxattrTooSmall{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	buf := make([]byte, 3)
	_, err = syscallx.Getxattr(mnt.Dir+"/child", "whatever", buf)
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
	fstestutil.File
}

func (f *getxattrSize) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	resp.Xattr = []byte("hello, world")
	return nil
}

func TestGetxattrSize(t *testing.T) {
	maybeParallel(t)
	f := &getxattrSize{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	n, err := syscallx.Getxattr(mnt.Dir+"/child", "whatever", nil)
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
	fstestutil.File
	record.Listxattrs
}

func (f *listxattr) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	f.Listxattrs.Listxattr(ctx, req, resp)
	resp.Append("one", "two")
	return nil
}

func TestListxattr(t *testing.T) {
	maybeParallel(t)
	f := &listxattr{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	buf := make([]byte, 8192)
	n, err := syscallx.Listxattr(mnt.Dir+"/child", buf)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	buf = buf[:n]
	if g, e := string(buf), "one\x00two\x00"; g != e {
		t.Errorf("wrong listxattr content: %#v != %#v", g, e)
	}

	want := fuse.ListxattrRequest{
		Size: 8192,
	}
	if g, e := f.RecordedListxattr(), want; g != e {
		t.Fatalf("listxattr saw %+v, want %+v", g, e)
	}
}

// Test Listxattr that has no space to return value

type listxattrTooSmall struct {
	fstestutil.File
}

func (f *listxattrTooSmall) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	resp.Xattr = []byte("one\x00two\x00")
	return nil
}

func TestListxattrTooSmall(t *testing.T) {
	maybeParallel(t)
	f := &listxattrTooSmall{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	buf := make([]byte, 3)
	_, err = syscallx.Listxattr(mnt.Dir+"/child", buf)
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
	fstestutil.File
}

func (f *listxattrSize) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	resp.Xattr = []byte("one\x00two\x00")
	return nil
}

func TestListxattrSize(t *testing.T) {
	maybeParallel(t)
	f := &listxattrSize{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	n, err := syscallx.Listxattr(mnt.Dir+"/child", nil)
	if err != nil {
		t.Errorf("Listxattr unexpected error: %v", err)
		return
	}
	if g, e := n, len("one\x00two\x00"); g != e {
		t.Errorf("Getxattr incorrect size: %d != %d", g, e)
	}
}

// Test Setxattr

type setxattr struct {
	fstestutil.File
	record.Setxattrs
}

func testSetxattr(t *testing.T, size int) {
	const linux_XATTR_NAME_MAX = 64 * 1024
	if size > linux_XATTR_NAME_MAX && runtime.GOOS == "linux" {
		t.Skip("large xattrs are not supported by linux")
	}

	maybeParallel(t)
	f := &setxattr{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	const g = "hello, world"
	greeting := strings.Repeat(g, size/len(g)+1)[:size]
	err = syscallx.Setxattr(mnt.Dir+"/child", "greeting", []byte(greeting), 0)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	// fuse.SetxattrRequest contains a byte slice and thus cannot be
	// directly compared
	got := f.RecordedSetxattr()

	if g, e := got.Name, "greeting"; g != e {
		t.Errorf("Setxattr incorrect name: %q != %q", g, e)
	}

	if g, e := got.Flags, uint32(0); g != e {
		t.Errorf("Setxattr incorrect flags: %d != %d", g, e)
	}

	if g, e := string(got.Xattr), greeting; g != e {
		t.Errorf("Setxattr incorrect data: %q != %q", g, e)
	}
}

func TestSetxattr(t *testing.T) {
	testSetxattr(t, 20)
}

func TestSetxattr64kB(t *testing.T) {
	testSetxattr(t, 64*1024)
}

func TestSetxattr16MB(t *testing.T) {
	testSetxattr(t, 16*1024*1024)
}

// Test Removexattr

type removexattr struct {
	fstestutil.File
	record.Removexattrs
}

func TestRemovexattr(t *testing.T) {
	maybeParallel(t)
	f := &removexattr{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	err = syscallx.Removexattr(mnt.Dir+"/child", "greeting")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	want := fuse.RemovexattrRequest{Name: "greeting"}
	if g, e := f.RecordedRemovexattr(), want; g != e {
		t.Errorf("removexattr saw %v, want %v", g, e)
	}
}

// Test default error.

type defaultErrno struct {
	fstestutil.Dir
}

func (f defaultErrno) Lookup(ctx context.Context, name string) (fs.Node, error) {
	return nil, errors.New("bork")
}

func TestDefaultErrno(t *testing.T) {
	maybeParallel(t)
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{defaultErrno{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	_, err = os.Stat(mnt.Dir + "/trigger")
	if err == nil {
		t.Fatalf("expected error")
	}

	switch err2 := err.(type) {
	case *os.PathError:
		if err2.Err == syscall.EIO {
			break
		}
		t.Errorf("unexpected inner error: Err=%v %#v", err2.Err, err2)
	default:
		t.Errorf("unexpected error: %v", err)
	}
}

// Test custom error.

type customErrNode struct {
	fstestutil.Dir
}

type myCustomError struct {
	fuse.ErrorNumber
}

var _ = fuse.ErrorNumber(myCustomError{})

func (myCustomError) Error() string {
	return "bork"
}

func (f customErrNode) Lookup(ctx context.Context, name string) (fs.Node, error) {
	return nil, myCustomError{
		ErrorNumber: fuse.Errno(syscall.ENAMETOOLONG),
	}
}

func TestCustomErrno(t *testing.T) {
	maybeParallel(t)
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{customErrNode{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	_, err = os.Stat(mnt.Dir + "/trigger")
	if err == nil {
		t.Fatalf("expected error")
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
}

// Test Mmap writing

type inMemoryFile struct {
	mu   sync.Mutex
	data []byte
}

func (f *inMemoryFile) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.data
}

func (f *inMemoryFile) Attr(ctx context.Context, a *fuse.Attr) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	a.Mode = 0666
	a.Size = uint64(len(f.data))
	return nil
}

func (f *inMemoryFile) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	fuseutil.HandleRead(req, resp, f.data)
	return nil
}

func (f *inMemoryFile) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	resp.Size = copy(f.data[req.Offset:], req.Data)
	return nil
}

const mmapSize = 16 * 4096

var mmapWrites = map[int]byte{
	10:              'a',
	4096:            'b',
	4097:            'c',
	mmapSize - 4096: 'd',
	mmapSize - 1:    'z',
}

func doMmap(ctx context.Context, dir string) (*struct{}, error) {
	f, err := os.Create(filepath.Join(dir, "child"))
	if err != nil {
		return nil, fmt.Errorf("Create: %v", err)
	}
	defer f.Close()
	data, err := syscall.Mmap(int(f.Fd()), 0, mmapSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("Mmap: %v", err)
	}
	for i, b := range mmapWrites {
		data[i] = b
	}
	if err := syscallx.Msync(data, syscall.MS_SYNC); err != nil {
		return nil, fmt.Errorf("Msync: %v", err)
	}
	if err := syscall.Munmap(data); err != nil {
		return nil, fmt.Errorf("Munmap: %v", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("Fsync = %v", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("Close: %v", err)
	}
	return &struct{}{}, nil
}

var mmapHelper = helpers.Register("mmap", httpjson.ServePOST(doMmap))

type mmap struct {
	inMemoryFile
	// We don't actually care about whether the fsync happened or not;
	// this just lets us force the page cache to send the writes to
	// FUSE, so we can reliably verify they came through.
	record.Fsyncs
}

func TestMmap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := &mmap{}
	w.data = make([]byte, mmapSize)
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": w}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	// Run the mmap-using parts of the test in a subprocess, to avoid
	// an intentional page fault hanging the whole process (because it
	// would need to be served by the same process, and there might
	// not be a thread free to do that). Merely bumping GOMAXPROCS is
	// not enough to prevent the hangs reliably.
	control := mmapHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	got := w.bytes()
	if g, e := len(got), mmapSize; g != e {
		t.Fatalf("bad write length: %d != %d", g, e)
	}
	for i, g := range got {
		// default '\x00' for writes[i] is good here
		if e := mmapWrites[i]; g != e {
			t.Errorf("wrong byte at offset %d: %q != %q", i, g, e)
		}
	}
}

// Test direct Read.

type directRead struct {
	fstestutil.File
}

// explicitly not defining Attr and setting Size

func (f directRead) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	// do not allow the kernel to use page cache
	resp.Flags |= fuse.OpenDirectIO
	return f, nil
}

func (directRead) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	fuseutil.HandleRead(req, resp, []byte(hi))
	return nil
}

func TestDirectRead(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": directRead{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readHelper.Spawn(ctx, t)
	defer control.Close()
	var got readResult
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := string(got.Data), hi; g != e {
		t.Errorf("readAll = %q, want %q", g, e)
	}
}

// Test direct Write.

type directWrite struct {
	fstestutil.File
	record.Writes
}

// explicitly not defining Attr / Setattr and managing Size

func (f *directWrite) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	// do not allow the kernel to use page cache
	resp.Flags |= fuse.OpenDirectIO
	return f, nil
}

func TestDirectWrite(t *testing.T) {
	maybeParallel(t)
	w := &directWrite{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": w}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	f, err := os.OpenFile(mnt.Dir+"/child", os.O_RDWR, 0666)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer f.Close()
	n, err := f.Write([]byte(hi))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(hi) {
		t.Fatalf("short write; n=%d; hi=%d", n, len(hi))
	}

	err = f.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := string(w.RecordedWriteData()); got != hi {
		t.Errorf("write = %q, want %q", got, hi)
	}
}

// Test Attr

// attrUnlinked is a file that is unlinked (Nlink==0).
type attrUnlinked struct {
	fstestutil.File
}

var _ fs.Node = attrUnlinked{}

func (f attrUnlinked) Attr(ctx context.Context, a *fuse.Attr) error {
	if err := f.File.Attr(ctx, a); err != nil {
		return err
	}
	a.Nlink = 0
	return nil
}

func TestAttrUnlinked(t *testing.T) {
	maybeParallel(t)
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": attrUnlinked{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	fi, err := os.Stat(mnt.Dir + "/child")
	if err != nil {
		t.Fatalf("Stat failed with %v", err)
	}
	switch stat := fi.Sys().(type) {
	case *syscall.Stat_t:
		if stat.Nlink != 0 {
			t.Errorf("wrong link count: %v", stat.Nlink)
		}
	}
}

// Test behavior when Attr method fails

type attrBad struct {
}

var _ fs.Node = attrBad{}

func (attrBad) Attr(ctx context.Context, attr *fuse.Attr) error {
	return fuse.Errno(syscall.ENAMETOOLONG)
}

func TestAttrBad(t *testing.T) {
	maybeParallel(t)
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": attrBad{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	_, err = os.Stat(mnt.Dir + "/child")
	if nerr, ok := err.(*os.PathError); !ok || nerr.Err != syscall.ENAMETOOLONG {
		t.Fatalf("wrong error: %v", err)
	}
}

// Test kernel cache invalidation

type invalidateAttr struct {
	fs.NodeRef
	t    testing.TB
	attr record.Counter
}

var _ fs.Node = (*invalidateAttr)(nil)

func (i *invalidateAttr) Attr(ctx context.Context, a *fuse.Attr) error {
	i.attr.Inc()
	i.t.Logf("Attr called, #%d", i.attr.Count())
	a.Mode = 0600
	return nil
}

func TestInvalidateNodeAttr(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	a := &invalidateAttr{
		t: t,
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": a}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	if !mnt.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	for i := 0; i < 10; i++ {
		if _, err := os.Stat(mnt.Dir + "/child"); err != nil {
			t.Fatalf("stat error: %v", err)
		}
	}
	// With OSXFUSE 3.0.4, we seem to see typically two Attr calls by
	// this point; something not populating the in-kernel cache
	// properly? Cope with it; we care more about seeing a new Attr
	// call after the invalidation.
	//
	// We still enforce a max number here so that we know that the
	// invalidate actually did something, and it's not just that every
	// Stat results in an Attr.
	before := a.attr.Count()
	if before == 0 {
		t.Error("no Attr call seen")
	}
	if g, e := before, uint32(3); g > e {
		t.Errorf("too many Attr calls seen: %d > %d", g, e)
	}

	t.Logf("invalidating...")
	if err := mnt.Server.InvalidateNodeAttr(a); err != nil {
		t.Fatalf("invalidate error: %v", err)
	}

	for i := 0; i < 10; i++ {
		if _, err := os.Stat(mnt.Dir + "/child"); err != nil {
			t.Fatalf("stat error: %v", err)
		}
	}
	if g, e := a.attr.Count(), before+1; g != e {
		t.Errorf("wrong Attr call count: %d != %d", g, e)
	}
}

type invalidateData struct {
	fs.NodeRef
	t    testing.TB
	attr record.Counter
	read record.Counter
	data atomic.Value
}

const (
	invalidateDataContent1 = "hello, world\n"
	invalidateDataContent2 = "so long!\n"
)

var _ fs.Node = (*invalidateData)(nil)

func (i *invalidateData) Attr(ctx context.Context, a *fuse.Attr) error {
	i.attr.Inc()
	i.t.Logf("Attr called, #%d", i.attr.Count())
	a.Mode = 0600
	a.Size = uint64(len(i.data.Load().(string)))
	return nil
}

var _ fs.HandleReader = (*invalidateData)(nil)

func (i *invalidateData) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	i.read.Inc()
	i.t.Logf("Read called, #%d", i.read.Count())
	fuseutil.HandleRead(req, resp, []byte(i.data.Load().(string)))
	return nil
}

func TestInvalidateNodeDataInvalidatesAttr(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	a := &invalidateData{
		t: t,
	}
	a.data.Store(invalidateDataContent1)
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": a}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	if !mnt.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	f, err := os.Open(mnt.Dir + "/child")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	attrBefore := a.attr.Count()
	if g, min := attrBefore, uint32(1); g < min {
		t.Errorf("wrong Attr call count: %d < %d", g, min)
	}

	t.Logf("invalidating...")
	a.data.Store(invalidateDataContent2)
	if err := mnt.Server.InvalidateNodeData(a); err != nil {
		t.Fatalf("invalidate error: %v", err)
	}

	// on OSXFUSE 3.0.6, the Attr has already triggered here, so don't
	// check the count at this point

	if _, err := f.Stat(); err != nil {
		t.Errorf("stat error: %v", err)
	}
	if g, prev := a.attr.Count(), attrBefore; g <= prev {
		t.Errorf("did not see Attr call after invalidate: %d <= %d", g, prev)
	}
}

func TestInvalidateNodeDataInvalidatesData(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	a := &invalidateData{
		t: t,
	}
	a.data.Store(invalidateDataContent1)
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": a}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	if !mnt.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	f, err := os.Open(mnt.Dir + "/child")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	{
		buf := make([]byte, 100)
		for i := 0; i < 10; i++ {
			n, err := f.ReadAt(buf, 0)
			if err != nil && err != io.EOF {
				t.Fatalf("readat error: %v", err)
			}
			if g, e := string(buf[:n]), invalidateDataContent1; g != e {
				t.Errorf("wrong content: %q != %q", g, e)
			}
		}
	}
	if g, e := a.read.Count(), uint32(1); g != e {
		t.Errorf("wrong Read call count: %d != %d", g, e)
	}

	t.Logf("invalidating...")
	a.data.Store(invalidateDataContent2)
	if err := mnt.Server.InvalidateNodeData(a); err != nil {
		t.Fatalf("invalidate error: %v", err)
	}

	if g, e := a.read.Count(), uint32(1); g != e {
		t.Errorf("wrong Read call count: %d != %d", g, e)
	}

	{
		// explicitly don't cross the EOF, to trigger more edge cases
		// (Linux will always do Getattr if you cross what it believes
		// the EOF to be)
		const bufSize = len(invalidateDataContent2) - 3
		buf := make([]byte, bufSize)
		for i := 0; i < 10; i++ {
			n, err := f.ReadAt(buf, 0)
			if err != nil && err != io.EOF {
				t.Fatalf("readat error: %v", err)
			}
			if g, e := string(buf[:n]), invalidateDataContent2[:bufSize]; g != e {
				t.Errorf("wrong content: %q != %q", g, e)
			}
		}
	}
	if g, e := a.read.Count(), uint32(2); g != e {
		t.Errorf("wrong Read call count: %d != %d", g, e)
	}
}

type invalidateDataPartial struct {
	fs.NodeRef
	t    testing.TB
	attr record.Counter
	read record.Counter
}

var invalidateDataPartialContent = strings.Repeat("hello, world\n", 1000)

var _ fs.Node = (*invalidateDataPartial)(nil)

func (i *invalidateDataPartial) Attr(ctx context.Context, a *fuse.Attr) error {
	i.attr.Inc()
	i.t.Logf("Attr called, #%d", i.attr.Count())
	a.Mode = 0600
	a.Size = uint64(len(invalidateDataPartialContent))
	return nil
}

var _ fs.HandleReader = (*invalidateDataPartial)(nil)

func (i *invalidateDataPartial) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	i.read.Inc()
	i.t.Logf("Read called, #%d", i.read.Count())
	fuseutil.HandleRead(req, resp, []byte(invalidateDataPartialContent))
	return nil
}

func TestInvalidateNodeDataRangeMiss(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	a := &invalidateDataPartial{
		t: t,
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": a}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	if !mnt.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	f, err := os.Open(mnt.Dir + "/child")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	buf := make([]byte, 4)
	for i := 0; i < 10; i++ {
		if _, err := f.ReadAt(buf, 0); err != nil {
			t.Fatalf("readat error: %v", err)
		}
	}
	if g, e := a.read.Count(), uint32(1); g != e {
		t.Errorf("wrong Read call count: %d != %d", g, e)
	}

	t.Logf("invalidating an uninteresting block...")
	if err := mnt.Server.InvalidateNodeDataRange(a, 4096, 4096); err != nil {
		t.Fatalf("invalidate error: %v", err)
	}

	for i := 0; i < 10; i++ {
		if _, err := f.ReadAt(buf, 0); err != nil {
			t.Fatalf("readat error: %v", err)
		}
	}
	// The page invalidated is not the page we're reading, so it
	// should stay in cache.
	if g, e := a.read.Count(), uint32(1); g != e {
		t.Errorf("wrong Read call count: %d != %d", g, e)
	}
}

func TestInvalidateNodeDataRangeHit(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	a := &invalidateDataPartial{
		t: t,
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": a}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	if !mnt.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	f, err := os.Open(mnt.Dir + "/child")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	const offset = 4096
	buf := make([]byte, 4)
	for i := 0; i < 10; i++ {
		if _, err := f.ReadAt(buf, offset); err != nil {
			t.Fatalf("readat error: %v", err)
		}
	}
	if g, e := a.read.Count(), uint32(1); g != e {
		t.Errorf("wrong Read call count: %d != %d", g, e)
	}

	t.Logf("invalidating where the reads are...")
	if err := mnt.Server.InvalidateNodeDataRange(a, offset, 4096); err != nil {
		t.Fatalf("invalidate error: %v", err)
	}

	for i := 0; i < 10; i++ {
		if _, err := f.ReadAt(buf, offset); err != nil {
			t.Fatalf("readat error: %v", err)
		}
	}
	// One new read
	if g, e := a.read.Count(), uint32(2); g != e {
		t.Errorf("wrong Read call count: %d != %d", g, e)
	}
}

type invalidateEntryRoot struct {
	fs.NodeRef
	t      testing.TB
	lookup record.Counter
}

var _ fs.Node = (*invalidateEntryRoot)(nil)

func (i *invalidateEntryRoot) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0600 | os.ModeDir
	return nil
}

var _ fs.NodeStringLookuper = (*invalidateEntryRoot)(nil)

func (i *invalidateEntryRoot) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if name != "child" {
		return nil, fuse.ENOENT
	}
	i.lookup.Inc()
	i.t.Logf("Lookup called, #%d", i.lookup.Count())
	return fstestutil.File{}, nil
}

func TestInvalidateEntry(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	a := &invalidateEntryRoot{
		t: t,
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{a}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	if !mnt.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	for i := 0; i < 10; i++ {
		if _, err := os.Stat(mnt.Dir + "/child"); err != nil {
			t.Fatalf("stat error: %v", err)
		}
	}
	if g, e := a.lookup.Count(), uint32(1); g != e {
		t.Errorf("wrong Lookup call count: %d != %d", g, e)
	}

	t.Logf("invalidating...")
	if err := mnt.Server.InvalidateEntry(a, "child"); err != nil {
		t.Fatalf("invalidate error: %v", err)
	}

	for i := 0; i < 10; i++ {
		if _, err := os.Stat(mnt.Dir + "/child"); err != nil {
			t.Fatalf("stat error: %v", err)
		}
	}
	if g, e := a.lookup.Count(), uint32(2); g != e {
		t.Errorf("wrong Lookup call count: %d != %d", g, e)
	}
}

type contextFile struct {
	fstestutil.File
}

var contextFileSentinel int

func (contextFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	v := ctx.Value(&contextFileSentinel)
	if v == nil {
		return nil, fuse.ESTALE
	}
	data, ok := v.(string)
	if !ok {
		return nil, fuse.EIO
	}
	resp.Flags |= fuse.OpenDirectIO
	return fs.DataHandle([]byte(data)), nil
}

func TestContext(t *testing.T) {
	maybeParallel(t)
	const input = "kilroy was here"
	mnt, err := fstestutil.MountedT(t,
		fstestutil.SimpleFS{&fstestutil.ChildMap{"child": contextFile{}}},
		&fs.Config{
			WithContext: func(ctx context.Context, req fuse.Request) context.Context {
				return context.WithValue(ctx, &contextFileSentinel, input)
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	data, err := ioutil.ReadFile(mnt.Dir + "/child")
	if err != nil {
		t.Fatalf("cannot read context file: %v", err)
	}
	if g, e := string(data), input; g != e {
		t.Errorf("read wrong data: %q != %q", g, e)
	}
}

type goexitFile struct {
	fstestutil.File
}

func (goexitFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	log.Println("calling runtime.Goexit...")
	runtime.Goexit()
	panic("not reached")
}

func TestGoexit(t *testing.T) {
	maybeParallel(t)
	mnt, err := fstestutil.MountedT(t,
		fstestutil.SimpleFS{&fstestutil.ChildMap{"child": goexitFile{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	_, err = ioutil.ReadFile(mnt.Dir + "/child")
	if nerr, ok := err.(*os.PathError); !ok || nerr.Err != syscall.EIO {
		t.Fatalf("wrong error from exiting handler: %T: %v", err, err)
	}
}
