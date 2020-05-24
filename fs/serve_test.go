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
	"golang.org/x/sys/unix"
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
}

func (f symlink) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeSymlink | 0o666
	return nil
}

// fifo can be embedded in a struct to make it look like a named pipe.
type fifo struct{}

func (f fifo) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeNamedPipe | 0o666
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
	a.Mode = os.ModeDir | 0o777
	return nil
}

func (f testPanic) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	panic(panicSentinel{})
}

func doPanic(ctx context.Context, dir string) (*struct{}, error) {
	err := os.Mkdir(dir+"/trigger-a-panic", 0o700)
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
	a.Mode = os.ModeDir | 0o777
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

type statfsResult struct {
	Blocks  uint64
	Bfree   uint64
	Bavail  uint64
	Files   uint64
	Ffree   uint64
	Bsize   int64
	Namelen int64
	Frsize  int64
}

func doStatfs(ctx context.Context, dir string) (*statfsResult, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return nil, fmt.Errorf("Statfs failed: %v", err)
	}
	log.Printf("Statfs got: %#v", st)
	r := platformStatfs(&st)
	return r, nil
}

var statfsHelper = helpers.Register("statfs", httpjson.ServePOST(doStatfs))

func testStatfs(t *testing.T, helper *spawntest.Helper) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, testStatFS{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()

	control := helper.Spawn(ctx, t)
	defer control.Close()
	var got statfsResult
	if err := control.JSON("/").Call(ctx, mnt.Dir, &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	if g, e := got.Blocks, uint64(42); g != e {
		t.Errorf("got Blocks = %d; want %d", g, e)
	}
	if g, e := got.Bfree, uint64(10); g != e {
		t.Errorf("got Bfree = %d; want %d", g, e)
	}
	if g, e := got.Bavail, uint64(3); g != e {
		t.Errorf("got Bavail = %d; want %d", g, e)
	}
	if g, e := got.Files, uint64(13); g != e {
		t.Errorf("got Files = %d; want %d", g, e)
	}
	if g, e := got.Ffree, uint64(11); g != e {
		t.Errorf("got Ffree = %d; want %d", g, e)
	}
	switch runtime.GOOS {
	case "freebsd":
		// freebsd gives 65536 here regardless of the fuse fs
		if got.Bsize != 65536 {
			t.Errorf("freebsd now implements statfs Bsize, please fix tests")
		}
	default:
		if g, e := got.Bsize, int64(1000); g != e {
			t.Errorf("got Bsize = %d; want %d", g, e)
		}
	}
	if g, e := got.Namelen, int64(34); g != e {
		t.Errorf("got Namelen = %d; want %d", g, e)
	}
	if g, e := got.Frsize, int64(7); g != e {
		t.Errorf("got Frsize = %d; want %d", g, e)
	}
}

func TestStatfs(t *testing.T) {
	testStatfs(t, statfsHelper)
}

func doFstatfs(ctx context.Context, dir string) (*statfsResult, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("Open for fstatfs failed: %v", err)
	}
	defer f.Close()
	var st syscall.Statfs_t
	err = syscall.Fstatfs(int(f.Fd()), &st)
	if err != nil {
		return nil, fmt.Errorf("Fstatfs failed: %v", err)
	}
	log.Printf("Fstatfs got: %#v", st)
	r := platformStatfs(&st)
	return r, nil
}

var fstatfsHelper = helpers.Register("fstatfs", httpjson.ServePOST(doFstatfs))

func TestFstatfs(t *testing.T) {
	testStatfs(t, fstatfsHelper)
}

// Test Stat of root.

type root struct{}

func (f root) Root() (fs.Node, error) {
	return f, nil
}

func (root) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = 1
	a.Mode = os.ModeDir | 0o555
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
	r := platformStat(fi)
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
	if p := got.Mode.Perm(); p != 0o555 {
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
	if g, e := got.Blksize, int64(65536); g != e {
		t.Errorf("root has wrong blocksize: %d != %d", g, e)
	}
}

// Test Read calling ReadAll.

type readAll struct {
	fstestutil.File
}

const hi = "hello, world"

func (readAll) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o666
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
	a.Mode = 0o666
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
	a.Mode = 0o666
	a.Size = uint64(len(hi))
	return nil
}

func (r *readFlags) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	r.fileFlags.Record(req.FileFlags)
	fuseutil.HandleRead(req, resp, []byte(hi))
	return nil
}

func doReadFileFlags(ctx context.Context, path string) (*struct{}, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o666)
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

	control := readFileFlagsHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	got := r.fileFlags.Recorded().(fuse.OpenFlags)
	got &^= fuse.OpenNonblock
	want := fuse.OpenReadWrite | fuse.OpenAppend
	if runtime.GOOS == "freebsd" {
		// FreeBSD doesn't pass append to FUSE?
		want ^= fuse.OpenAppend
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
	a.Mode = 0o666
	// do not set Size here or FreeBSD will do a read-modify-write,
	// even if the write replaces whole page contents
	return nil
}

func (r *writeFlags) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	r.fileFlags.Record(req.FileFlags)
	resp.Size = len(req.Data)
	return nil
}

func doWriteFileFlags(ctx context.Context, path string) (*struct{}, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o666)
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

	control := writeFileFlagsHelper.Spawn(ctx, t)
	defer control.Close()
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	got := r.fileFlags.Recorded().(fuse.OpenFlags)
	got &^= fuse.OpenNonblock
	want := fuse.OpenReadWrite | fuse.OpenAppend
	if runtime.GOOS == "freebsd" {
		// FreeBSD doesn't pass append to FUSE?
		want &^= fuse.OpenAppend
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
	got, ok := r.WaitForRelease(1 * time.Second)
	if !ok {
		t.Error("Close did not Release in time")
	}
	// dynamic values that are too hard to control
	if got.Handle == 0 {
		t.Errorf("got ReleaseRequest with no Handle")
	}
	got.Handle = 0
	want := &fuse.ReleaseRequest{
		Flags: fuse.OpenReadOnly | fuse.OpenNonblock,
	}
	if runtime.GOOS == "freebsd" {
		// Go on FreeBSD isn't using the netpoller for os.File?
		want.Flags &^= fuse.OpenNonblock
		// no locking used but FreeBSD sets LockOwner?
		got.LockOwner = 0
	}
	if g, e := got, want; *g != *e {
		t.Errorf("bad release:\ngot\t%v\nwant\t%v", g, e)
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
	if err := ioutil.WriteFile(req.Path, req.Data, 0o666); err != nil {
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
	syscall.Umask(0o022)
	if err := os.Mkdir(path, 0o771); err != nil {
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

	want := fuse.MkdirRequest{
		Name:  "foo",
		Mode:  os.ModeDir | 0o751,
		Umask: 0o022,
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
		return nil, nil, syscall.EPERM
	}

	_, _, _ = f.f.Creates.Create(ctx, req, resp)
	return &f.f, &f.f, nil
}

func doCreate(ctx context.Context, path string) (*struct{}, error) {
	// uniform umask needed to make os.Mkdir's mode into something
	// reproducible
	syscall.Umask(0o022)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o640)
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
		Mode:  0o640,
		Umask: 0o022,
	}
	if runtime.GOOS == "freebsd" {
		// FreeBSD doesn't pass truncate to FUSE?; as this is a
		// Create, that's acceptable
		want.Flags &^= fuse.OpenTruncate
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
		return nil, nil, syscall.EPERM
	}
	f.fooCreated.Mark()
	return &f.f, &f.f, nil
}

func (f *create3) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if f.fooCreated.Recorded() && !f.fooRemoved.Recorded() && name == "foo" {
		return &f.f, nil
	}
	return nil, syscall.ENOENT
}

func (f *create3) Remove(ctx context.Context, r *fuse.RemoveRequest) error {
	if f.fooCreated.Recorded() && !f.fooRemoved.Recorded() &&
		r.Name == "foo" && !r.Dir {
		f.fooRemoved.Mark()
		return nil
	}
	return syscall.ENOENT
}

func doCreateWriteRemove(ctx context.Context, path string) (*struct{}, error) {
	if err := ioutil.WriteFile(path, []byte(hi), 0o666); err != nil {
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
	fs *symlink1
}

func (f symlink1link) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (string, error) {
	return f.fs.RecordedSymlink().Target, nil
}

type symlink1 struct {
	fstestutil.Dir
	record.Symlinks
}

var _ fs.NodeStringLookuper = (*symlink1)(nil)

func (f *symlink1) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if name != "symlink.file" {
		return nil, syscall.ENOENT
	}
	if f.RecordedSymlink() == (fuse.SymlinkRequest{}) {
		return nil, syscall.ENOENT
	}
	return symlink1link{fs: f}, nil
}

func (f *symlink1) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fs.Node, error) {
	if f.RecordedSymlink() != (fuse.SymlinkRequest{}) {
		log.Print("this test is not prepared to handle multiple symlinks")
		return nil, fuse.Errno(syscall.ENAMETOOLONG)
	}
	f.Symlinks.Symlink(ctx, req)
	return symlink1link{fs: f}, nil
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
	return nil, syscall.ENOENT
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
	return nil, syscall.ENOENT
}

func (f *rename1) Rename(ctx context.Context, r *fuse.RenameRequest, newDir fs.Node) error {
	if r.OldName == "old" && r.NewName == "new" && newDir == f {
		f.renamed.Inc()
		return nil
	}
	return syscall.EIO
}

type renameRequest struct {
	OldName   string
	NewName   string
	WantErrno syscall.Errno
}

func doRename(ctx context.Context, req renameRequest) (*struct{}, error) {
	var want error
	if req.WantErrno > 0 {
		want = req.WantErrno
	}
	if err := os.Rename(req.OldName, req.NewName); !errors.Is(err, want) {
		return nil, fmt.Errorf("wrong error: %v", err)
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
	syscall.Umask(0o022)
	if err := syscall.Mknod(path, syscall.S_IFIFO|0o660, 123); err != nil {
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
		Name:  "node",
		Mode:  os.FileMode(os.ModeNamedPipe | 0o640),
		Rdev:  uint32(123),
		Umask: 0o022,
	}
	if runtime.GOOS == "linux" {
		// Linux fuse doesn't echo back the rdev if the node
		// isn't a device (we're using a FIFO here, as that
		// bit is portable.)
		want.Rdev = 0
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
	a.Mode = 0o666
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
	a.Mode = 0o666
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
	if runtime.GOOS == "freebsd" {
		t.Skip("don't know how to trigger EINTR from read syscall on FreeBSD")
	}
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
	Flags     int
	Perm      os.FileMode
	WantErrno syscall.Errno
}

func doOpenErr(ctx context.Context, req openRequest) (*struct{}, error) {
	f, err := os.OpenFile(req.Path, req.Flags, req.Perm)
	if err == nil {
		f.Close()
	}
	if !errors.Is(err, req.WantErrno) {
		return nil, fmt.Errorf("wrong error: %v", err)
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
		Path:  mnt.Dir + "/child",
		Flags: os.O_RDONLY,
		Perm:  0,
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
	f, err := os.OpenFile(req.Path, os.O_WRONLY, 0o666)
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

func doTruncateWithOpen(ctx context.Context, path string) (*struct{}, error) {
	fil, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o666)
	if err != nil {
		return nil, err
	}
	_ = fil.Close()
	return &struct{}{}, nil
}

var truncateWithOpenHelper = helpers.Register("truncateWithOpen", httpjson.ServePOST(doTruncateWithOpen))

func TestTruncateWithOpen(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &truncateWithOpen{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := truncateWithOpenHelper.Spawn(ctx, t)
	defer control.Close()

	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	gotr := f.RecordedSetattr()
	if gotr == (fuse.SetattrRequest{}) {
		t.Fatalf("no recorded SetattrRequest")
	}
	if g, e := gotr.Size, uint64(0); g != e {
		t.Errorf("got Size = %q; want %q", g, e)
	}
	got := gotr.Valid
	if runtime.GOOS == "freebsd" {
		// FreeBSD seems to set this but Linux doesn't??? Want to
		// detect if Linux starts adding it. I assume the logic is
		// something like the truncate happens before the open; or it
		// just slipped by.
		got &^= fuse.SetattrHandle
	}
	if g, e := got&^fuse.SetattrLockOwner, fuse.SetattrSize; g != e {
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

func doReaddir(ctx context.Context, path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// go Readdir is just Readdirnames + Lstat, there's no point in
	// testing that here; we have no consumption API for the real
	// dirent data
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	return names, nil
}

var readdirHelper = helpers.Register("readdir", httpjson.ServePOST(doReaddir))

func TestReadDirAll(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &readDirAll{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readdirHelper.Spawn(ctx, t)
	defer control.Close()

	var names []string
	if err := control.JSON("/").Call(ctx, mnt.Dir, &names); err != nil {
		t.Fatalf("calling helper: %v", err)
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

func doReaddirBad(ctx context.Context, path string) (*struct{}, error) {
	fil, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fil.Close()

	var names []string
	for {
		n, err := fil.Readdirnames(1)
		if err != nil {
			if !errors.Is(err, syscall.ENAMETOOLONG) {
				return nil, fmt.Errorf("wrong error: %v", err)
			}
			break
		}
		names = append(names, n...)
	}

	log.Printf("Got readdir: %q", names)

	// TODO could serve partial results from ReadDirAll but the
	// shandle.readData mechanism makes that awkward.
	if len(names) != 0 {
		return nil, fmt.Errorf("expected 0 entries, got: %q", names)
	}
	return &struct{}{}, nil
}

var readdirBadHelper = helpers.Register("readdirBad", httpjson.ServePOST(doReaddirBad))

func TestReadDirAllBad(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &readDirAllBad{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readdirBadHelper.Spawn(ctx, t)
	defer control.Close()

	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

// Test readdir without any ReadDir methods implemented.

type readDirNotImplemented struct {
	fstestutil.Dir
}

func TestReadDirNotImplemented(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &readDirNotImplemented{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readdirHelper.Spawn(ctx, t)
	defer control.Close()

	var names []string
	if err := control.JSON("/").Call(ctx, mnt.Dir, &names); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	t.Logf("Got readdir: %q", names)

	if len(names) != 0 {
		t.Errorf(`expected 0 entries, got: %q`, names)
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

type readdirRewindHelp struct {
	mu   sync.Mutex
	file *os.File
}

func (r *readdirRewindHelp) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/openReaddir":
		httpjson.ServePOST(r.doOpenReaddir).ServeHTTP(w, req)
	case "/rewindReaddirClose":
		httpjson.ServePOST(r.doRewindReaddirClose).ServeHTTP(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (r *readdirRewindHelp) doOpenReaddir(ctx context.Context, path string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r.file = f
	names, err := f.Readdirnames(100)
	if err != nil {
		return nil, err
	}
	return names, nil
}

func (r *readdirRewindHelp) doRewindReaddirClose(ctx context.Context, _ struct{}) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.file.Close()
	if _, err := r.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	names, err := r.file.Readdirnames(100)
	if err != nil {
		return nil, err
	}
	return names, nil
}

var readdirRewindHelper = helpers.Register("readdirRewind", &readdirRewindHelp{})

func TestReadDirAllRewind(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &readDirAllRewind{}
	f.entries.Store([]fuse.Dirent{
		{Name: "one", Inode: 11, Type: fuse.DT_Dir},
	})
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readdirRewindHelper.Spawn(ctx, t)
	defer control.Close()

	{
		var names []string
		if err := control.JSON("/openReaddir").Call(ctx, mnt.Dir, &names); err != nil {
			t.Fatalf("calling helper: %v", err)
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
	{
		var names []string
		if err := control.JSON("/rewindReaddirClose").Call(ctx, struct{}{}, &names); err != nil {
			t.Fatalf("calling helper: %v", err)
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
		return syscall.EIO
	}
	f.Setattrs.Setattr(ctx, req, resp)
	return nil
}

type chmodRequest struct {
	Path string
	Mode os.FileMode
}

func doChmod(ctx context.Context, req chmodRequest) (*struct{}, error) {
	if err := os.Chmod(req.Path, req.Mode); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

var chmodHelper = helpers.Register("chmod", httpjson.ServePOST(doChmod))

func TestChmod(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &chmod{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := chmodHelper.Spawn(ctx, t)
	defer control.Close()

	req := chmodRequest{
		Path: mnt.Dir + "/child",
		Mode: 0o764,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	got := f.RecordedSetattr()
	if g, e := got.Mode.Perm(), os.FileMode(0o764); g != e {
		t.Errorf("wrong mode: %o %v != %o %v", g, g, e, e)
	}
	ftype := got.Mode & os.ModeType
	switch {
	case runtime.GOOS == "freebsd" && ftype == os.ModeIrregular:
		// acceptable but unfortunate
	default:
		if !ftype.IsRegular() {
			t.Errorf("mode is not regular: %o %v", got.Mode, got.Mode)
		}
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &open{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := openErrHelper.Spawn(ctx, t)
	defer control.Close()

	req := openRequest{
		Path:  mnt.Dir + "/child",
		Flags: os.O_WRONLY | os.O_APPEND,
		// note: mode only matters with O_CREATE
		Perm:      0,
		WantErrno: syscall.ENAMETOOLONG,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	want := fuse.OpenRequest{Dir: false, Flags: fuse.OpenWriteOnly | fuse.OpenAppend}
	got := f.RecordedOpen()

	if runtime.GOOS == "linux" {
		// Linux <3.7 accidentally leaks O_CLOEXEC through to FUSE;
		// avoid spurious test failures
		got.Flags &^= fuse.OpenFlags(syscall.O_CLOEXEC)
	}
	if runtime.GOOS == "freebsd" {
		// FreeBSD doesn't pass append to FUSE?
		want.Flags &^= fuse.OpenAppend
	}

	if g, e := got, want; g != e {
		t.Errorf("open saw %+v, want %+v", g, e)
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

func doOpenNonseekable(ctx context.Context, path string) (*struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekStart); !errors.Is(err, syscall.ESPIPE) {
		return nil, fmt.Errorf("wrong error: %v", err)
	}
	return &struct{}{}, nil
}

var openNonseekableHelper = helpers.Register("openNonseekable", httpjson.ServePOST(doOpenNonseekable))

func TestOpenNonSeekable(t *testing.T) {
	if runtime.GOOS == "freebsd" {
		// behavior observed: seek calls succeed, but file offset does
		// not change
		t.Skip("FreeBSD seems to ignore OpenNonSeekable")
	}

	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &openNonSeekable{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := openNonseekableHelper.Spawn(ctx, t)
	defer control.Close()

	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

// Test Fsync on a dir

type fsyncDir struct {
	fstestutil.Dir
	record.Fsyncs
}

func doOpenFsyncClose(ctx context.Context, path string) (*struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	err = f.Sync()
	if err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

var openFsyncCloseHelper = helpers.Register("openFsyncClose", httpjson.ServePOST(doOpenFsyncClose))

func TestFsyncDir(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fsyncDir{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := openFsyncCloseHelper.Spawn(ctx, t)
	defer control.Close()

	var nothing struct{}
	if err := control.JSON("/").Call(ctx, mnt.Dir, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	got := f.RecordedFsync()
	want := fuse.FsyncRequest{
		Flags: 0,
		Dir:   true,
		// unpredictable
		Handle: got.Handle,
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

type getxattrRequest struct {
	Path      string
	Name      string
	Size      int
	WantErrno syscall.Errno
}

type getxattrResult struct {
	// only one of Data and Size is set

	Data []byte
	Size int
}

func doGetxattr(ctx context.Context, req getxattrRequest) (*getxattrResult, error) {
	buf := make([]byte, req.Size)
	n, err := unix.Getxattr(req.Path, req.Name, buf)
	if req.WantErrno != 0 {
		if !errors.Is(err, req.WantErrno) {
			return nil, fmt.Errorf("wrong error: %v", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("unexpected error: %v", err)
	}
	if req.Size == 0 {
		r := &getxattrResult{
			Size: n,
		}
		return r, nil
	}
	r := &getxattrResult{
		Data: buf[:n],
	}
	return r, nil
}

var getxattrHelper = helpers.Register("getxattr", httpjson.ServePOST(doGetxattr))

func TestGetxattr(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &getxattr{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := getxattrHelper.Spawn(ctx, t)
	defer control.Close()

	req := getxattrRequest{
		Path: mnt.Dir + "/child",
		Name: "user.dummyxattr",
		Size: 8192,
	}
	var res getxattrResult
	if err := control.JSON("/").Call(ctx, req, &res); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := string(res.Data), "hello, world"; g != e {
		t.Errorf("wrong getxattr content: %#v != %#v", g, e)
	}
	seen := f.RecordedGetxattr()
	if g, e := seen.Name, "user.dummyxattr"; g != e {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &getxattrTooSmall{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := getxattrHelper.Spawn(ctx, t)
	defer control.Close()

	req := getxattrRequest{
		Path:      mnt.Dir + "/child",
		Name:      "user.dummyxattr",
		Size:      3,
		WantErrno: syscall.ERANGE,
	}
	var res getxattrResult
	if err := control.JSON("/").Call(ctx, req, &res); err != nil {
		t.Fatalf("calling helper: %v", err)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &getxattrSize{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := getxattrHelper.Spawn(ctx, t)
	defer control.Close()

	req := getxattrRequest{
		Path: mnt.Dir + "/child",
		Name: "user.dummyxattr",
		Size: 0,
	}
	var res getxattrResult
	if err := control.JSON("/").Call(ctx, req, &res); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := res.Size, len("hello, world"); g != e {
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
	resp.Append("user.one", "user.two")
	return nil
}

type listxattrRequest struct {
	Path      string
	Size      int
	WantErrno syscall.Errno
}

type listxattrResult struct {
	// only one of Data and Size is set

	Data []byte
	Size int
}

func doListxattr(ctx context.Context, req listxattrRequest) (*listxattrResult, error) {
	buf := make([]byte, req.Size)
	n, err := unix.Listxattr(req.Path, buf)
	if req.WantErrno != 0 {
		if !errors.Is(err, req.WantErrno) {
			return nil, fmt.Errorf("wrong error: %v", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("unexpected error: %v", err)
	}
	if req.Size == 0 {
		r := &listxattrResult{
			Size: n,
		}
		return r, nil
	}
	buf = buf[:n]

	if runtime.GOOS == "freebsd" {
		// Normalize FreeBSD listxattr syscall response to the same
		// zero-terminated format as others. This is just the
		// client-side syscall; the FUSE interaction still uses the
		// nil-terminated strings with namespace prefixes.
		//
		// Length-prefixed, no namespace (you have to query per
		// namespace on FreeBSD).
		var out []byte
		for len(buf) > 0 {
			size := int(buf[0])
			out = append(out, []byte("user.")...)
			out = append(out, buf[1:1+size]...)
			out = append(out, '\x00')
			buf = buf[1+size:]
		}
		buf = out
	}

	r := &listxattrResult{
		Data: buf,
	}
	return r, nil
}

var listxattrHelper = helpers.Register("listxattr", httpjson.ServePOST(doListxattr))

func TestListxattr(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &listxattr{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := listxattrHelper.Spawn(ctx, t)
	defer control.Close()

	req := listxattrRequest{
		Path: mnt.Dir + "/child",
		Size: 8192,
	}
	var res listxattrResult
	if err := control.JSON("/").Call(ctx, req, &res); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := string(res.Data), "user.one\x00user.two\x00"; g != e {
		t.Errorf("wrong listxattr content: %#v != %#v", g, e)
	}

	want := fuse.ListxattrRequest{
		Size: 8192,
	}
	if runtime.GOOS == "freebsd" {
		// FreeBSD seems to always probe the size for you, even when
		// userspace passed a large enough buffer. This means two (or
		// more, if the size keeps growing!) Listxattr FUSE requests,
		// with the last one likely having the perfect size (except
		// when the size changed downward between the calls). Blargh.
		want.Size = uint32(len("user.one\x00user.two\x00"))
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
	if runtime.GOOS == "freebsd" {
		t.Skip("FreeBSD xattr list format is different and the kernel has intermediate buffer; can't drive FUSE requests directly from userspace")
	}
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &listxattrTooSmall{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := listxattrHelper.Spawn(ctx, t)
	defer control.Close()

	req := listxattrRequest{
		Path:      mnt.Dir + "/child",
		Size:      3,
		WantErrno: syscall.ERANGE,
	}
	var res listxattrResult
	if err := control.JSON("/").Call(ctx, req, &res); err != nil {
		t.Fatalf("calling helper: %v", err)
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
	if runtime.GOOS == "freebsd" {
		t.Skip("FreeBSD xattr list format is different and the kernel has intermediate buffer; can't drive FUSE requests directly from userspace")
	}
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &listxattrSize{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := listxattrHelper.Spawn(ctx, t)
	defer control.Close()

	req := listxattrRequest{
		Path: mnt.Dir + "/child",
		Size: 0,
	}
	var res listxattrResult
	if err := control.JSON("/").Call(ctx, req, &res); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := res.Size, len("one\x00two\x00"); g != e {
		t.Errorf("Listxattr incorrect size: %d != %d", g, e)
	}
}

// Test Setxattr

type setxattr struct {
	fstestutil.File
	record.Setxattrs
}

type setxattrRequest struct {
	Path  string
	Name  string
	Data  []byte
	Flags int
}

func doSetxattr(ctx context.Context, req setxattrRequest) (*struct{}, error) {
	if err := unix.Setxattr(req.Path, req.Name, req.Data, req.Flags); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

var setxattrHelper = helpers.Register("setxattr", httpjson.ServePOST(doSetxattr))

func testSetxattr(t *testing.T, size int) {
	const linux_XATTR_NAME_MAX = 64 * 1024
	if size > linux_XATTR_NAME_MAX && runtime.GOOS == "linux" {
		t.Skip("large xattrs are not supported by linux")
	}
	if runtime.GOOS == "freebsd" && size > 135106 {
		// no idea what that magic number is but it seems like a very
		// repeatable exact cutoff for me
		t.Skip("FreeBSD setxattr seems to hang on large values")
	}

	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &setxattr{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := setxattrHelper.Spawn(ctx, t)
	defer control.Close()

	const g = "hello, world"
	greeting := strings.Repeat(g, size/len(g)+1)[:size]
	req := setxattrRequest{
		Path:  mnt.Dir + "/child",
		Name:  "user.greeting",
		Data:  []byte(greeting),
		Flags: 0,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	// fuse.SetxattrRequest contains a byte slice and thus cannot be
	// directly compared
	got := f.RecordedSetxattr()

	if g, e := got.Name, "user.greeting"; g != e {
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
	t.Run("20", func(t *testing.T) { testSetxattr(t, 20) })
	t.Run("64kB", func(t *testing.T) { testSetxattr(t, 64*1024) })
	t.Run("16MB", func(t *testing.T) { testSetxattr(t, 16*1024*1024) })
}

// Test Removexattr

type removexattr struct {
	fstestutil.File
	record.Removexattrs
}

type removexattrRequest struct {
	Path string
	Name string
}

func doRemovexattr(ctx context.Context, req removexattrRequest) (*struct{}, error) {
	if err := unix.Removexattr(req.Path, req.Name); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

var removexattrHelper = helpers.Register("removexattr", httpjson.ServePOST(doRemovexattr))

func TestRemovexattr(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &removexattr{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": f}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := removexattrHelper.Spawn(ctx, t)
	defer control.Close()

	req := removexattrRequest{
		Path: mnt.Dir + "/child",
		Name: "user.greeting",
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	want := fuse.RemovexattrRequest{Name: "user.greeting"}
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

type statErrRequest struct {
	Path      string
	WantErrno syscall.Errno
}

func doStatErr(ctx context.Context, req statErrRequest) (*struct{}, error) {
	if _, err := os.Stat(req.Path); !errors.Is(err, req.WantErrno) {
		return nil, fmt.Errorf("wrong error: %v", err)
	}
	return &struct{}{}, nil
}

var statErrHelper = helpers.Register("statErr", httpjson.ServePOST(doStatErr))

func TestDefaultErrno(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{defaultErrno{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := statErrHelper.Spawn(ctx, t)
	defer control.Close()

	req := statErrRequest{
		Path:      mnt.Dir + "/child",
		WantErrno: syscall.EIO,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

// Test custom error.

type customErrNode struct {
	fstestutil.Dir
}

type myCustomError struct {
	fuse.ErrorNumber
}

var _ fuse.ErrorNumber = myCustomError{}

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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{customErrNode{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := statErrHelper.Spawn(ctx, t)
	defer control.Close()

	req := statErrRequest{
		Path:      mnt.Dir + "/child",
		WantErrno: syscall.ENAMETOOLONG,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

// Test returning syscall.Errno

type syscallErrNode struct {
	fstestutil.Dir
}

func (f syscallErrNode) Lookup(ctx context.Context, name string) (fs.Node, error) {
	return nil, syscall.ENAMETOOLONG
}

func TestSyscallErrno(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{syscallErrNode{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := statErrHelper.Spawn(ctx, t)
	defer control.Close()

	req := statErrRequest{
		Path:      mnt.Dir + "/child",
		WantErrno: syscall.ENAMETOOLONG,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
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

	a.Mode = 0o666
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
	if err := unix.Msync(data, syscall.MS_SYNC); err != nil {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &directWrite{}
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": attrUnlinked{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := statHelper.Spawn(ctx, t)
	defer control.Close()

	var got statResult
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := got.Nlink, uint64(0); g != e {
		t.Errorf("wrong link count: %v != %v", g, e)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": attrBad{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := statErrHelper.Spawn(ctx, t)
	defer control.Close()

	req := statErrRequest{
		Path:      mnt.Dir + "/child",
		WantErrno: syscall.ENAMETOOLONG,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

// Test kernel cache invalidation

type invalidateAttr struct {
	t    testing.TB
	attr record.Counter
}

var _ fs.Node = (*invalidateAttr)(nil)

func (i *invalidateAttr) Attr(ctx context.Context, a *fuse.Attr) error {
	i.attr.Inc()
	i.t.Logf("Attr called, #%d", i.attr.Count())
	a.Mode = 0o600
	return nil
}

func TestInvalidateNodeAttr(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &invalidateAttr{
		t: t,
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": a}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := statHelper.Spawn(ctx, t)
	defer control.Close()

	for i := 0; i < 10; i++ {
		var got statResult
		if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
			t.Fatalf("calling helper: %v", err)
		}
	}
	before := a.attr.Count()
	if before == 0 {
		t.Error("no Attr call seen")
	}
	if g, e := before, uint32(1); g > e {
		t.Errorf("too many Attr calls seen: %d > %d", g, e)
	}

	t.Logf("invalidating...")
	if err := mnt.Server.InvalidateNodeAttr(a); err != nil {
		t.Fatalf("invalidate error: %v", err)
	}

	for i := 0; i < 10; i++ {
		var got statResult
		if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
			t.Fatalf("calling helper: %v", err)
		}
	}
	if g, e := a.attr.Count(), before+1; g != e {
		t.Errorf("wrong Attr call count: %d != %d", g, e)
	}
}

type invalidateData struct {
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
	a.Mode = 0o600
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

type fstatHelp struct {
	mu   sync.Mutex
	file *os.File
}

func (f *fstatHelp) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/open":
		httpjson.ServePOST(f.doOpen).ServeHTTP(w, req)
	case "/fstat":
		httpjson.ServePOST(f.doFstat).ServeHTTP(w, req)
	case "/close":
		httpjson.ServePOST(f.doClose).ServeHTTP(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (f *fstatHelp) doOpen(ctx context.Context, path string) (*struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fil, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	f.file = fil
	return &struct{}{}, nil
}

func (f *fstatHelp) doFstat(ctx context.Context, _ struct{}) (*statResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, err := f.file.Stat()
	if err != nil {
		return nil, err
	}
	r := platformStat(fi)
	return r, nil
}

func (f *fstatHelp) doClose(ctx context.Context, _ struct{}) (*struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.file.Close()
	return &struct{}{}, nil
}

var fstatHelper = helpers.Register("fstat", &fstatHelp{})

func TestInvalidateNodeDataInvalidatesAttr(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &invalidateData{
		t: t,
	}
	a.data.Store(invalidateDataContent1)
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": a}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := fstatHelper.Spawn(ctx, t)
	defer control.Close()

	var nothing struct{}
	if err := control.JSON("/open").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	attrBefore := a.attr.Count()
	if g, min := attrBefore, uint32(1); g < min {
		t.Errorf("wrong Attr call count: %d < %d", g, min)
	}

	t.Logf("invalidating...")
	a.data.Store(invalidateDataContent2)
	if err := mnt.Server.InvalidateNodeData(a); err != nil {
		t.Fatalf("invalidate error: %v", err)
	}

	if g, prev := a.attr.Count(), attrBefore; g != prev {
		t.Errorf("invalidate caused an Attr call: %d != %d", g, prev)
	}

	var got statResult
	if err := control.JSON("/fstat").Call(ctx, struct{}{}, &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, prev := a.attr.Count(), attrBefore; g != prev+1 {
		t.Errorf("none or too many Attr calls after stat: %d != %d+1", g, prev)
	}
}

type manyReadsHelp struct {
	mu   sync.Mutex
	file *os.File
}

func (m *manyReadsHelp) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/open":
		httpjson.ServePOST(m.doOpen).ServeHTTP(w, req)
	case "/readAt":
		httpjson.ServePOST(m.doReadAt).ServeHTTP(w, req)
	case "/close":
		httpjson.ServePOST(m.doClose).ServeHTTP(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (m *manyReadsHelp) doOpen(ctx context.Context, path string) (*struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fil, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	m.file = fil
	return &struct{}{}, nil
}

type readAtRequest struct {
	Offset int64
	Length int
}

func (m *manyReadsHelp) doReadAt(ctx context.Context, req readAtRequest) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := make([]byte, req.Length)
	n, err := m.file.ReadAt(buf, req.Offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	buf = buf[:n]
	return buf, nil
}

func (m *manyReadsHelp) doClose(ctx context.Context, _ struct{}) (*struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.file.Close()
	return &struct{}{}, nil
}

var manyReadsHelper = helpers.Register("manyReads", &manyReadsHelp{})

func TestInvalidateNodeDataInvalidatesData(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &invalidateData{
		t: t,
	}
	a.data.Store(invalidateDataContent1)
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": a}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := manyReadsHelper.Spawn(ctx, t)
	defer control.Close()

	var nothing struct{}
	if err := control.JSON("/open").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	{
		for i := 0; i < 10; i++ {
			req := readAtRequest{
				Offset: 0,
				Length: 100,
			}
			var got []byte
			if err := control.JSON("/readAt").Call(ctx, req, &got); err != nil {
				t.Fatalf("calling helper: %v", err)
			}
			if g, e := string(got), invalidateDataContent1; g != e {
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
		for i := 0; i < 10; i++ {
			req := readAtRequest{
				Offset: 0,
				Length: bufSize,
			}
			var got []byte
			if err := control.JSON("/readAt").Call(ctx, req, &got); err != nil {
				t.Fatalf("calling helper: %v", err)
			}
			if g, e := string(got), invalidateDataContent2[:bufSize]; g != e {
				t.Errorf("wrong content: %q != %q", g, e)
			}
		}
	}
	if g, e := a.read.Count(), uint32(2); g != e {
		t.Errorf("wrong Read call count: %d != %d", g, e)
	}
}

type invalidateDataPartial struct {
	t    testing.TB
	attr record.Counter
	read record.Counter
}

var invalidateDataPartialContent = strings.Repeat("hello, world\n", 1000)

var _ fs.Node = (*invalidateDataPartial)(nil)

func (i *invalidateDataPartial) Attr(ctx context.Context, a *fuse.Attr) error {
	i.attr.Inc()
	i.t.Logf("Attr called, #%d", i.attr.Count())
	a.Mode = 0o600
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
	if runtime.GOOS == "freebsd" {
		t.Skip("FreeBSD seems to always invalidate whole file")
	}
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &invalidateDataPartial{
		t: t,
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": a}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := manyReadsHelper.Spawn(ctx, t)
	defer control.Close()

	var nothing struct{}
	if err := control.JSON("/open").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	for i := 0; i < 10; i++ {
		req := readAtRequest{
			Offset: 0,
			Length: 4,
		}
		var got []byte
		if err := control.JSON("/readAt").Call(ctx, req, &got); err != nil {
			t.Fatalf("calling helper: %v", err)
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
		req := readAtRequest{
			Offset: 0,
			Length: 4,
		}
		var got []byte
		if err := control.JSON("/readAt").Call(ctx, req, &got); err != nil {
			t.Fatalf("calling helper: %v", err)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &invalidateDataPartial{
		t: t,
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": a}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := manyReadsHelper.Spawn(ctx, t)
	defer control.Close()

	var nothing struct{}
	if err := control.JSON("/open").Call(ctx, mnt.Dir+"/child", &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	const offset = 4096
	for i := 0; i < 10; i++ {
		req := readAtRequest{
			Offset: offset,
			Length: 4,
		}
		var got []byte
		if err := control.JSON("/readAt").Call(ctx, req, &got); err != nil {
			t.Fatalf("calling helper: %v", err)
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
		req := readAtRequest{
			Offset: offset,
			Length: 4,
		}
		var got []byte
		if err := control.JSON("/readAt").Call(ctx, req, &got); err != nil {
			t.Fatalf("calling helper: %v", err)
		}
	}
	// One new read
	if g, e := a.read.Count(), uint32(2); g != e {
		t.Errorf("wrong Read call count: %d != %d", g, e)
	}
}

type invalidateEntryRoot struct {
	t      testing.TB
	lookup record.Counter
}

var _ fs.Node = (*invalidateEntryRoot)(nil)

func (i *invalidateEntryRoot) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o600 | os.ModeDir
	return nil
}

var _ fs.NodeStringLookuper = (*invalidateEntryRoot)(nil)

func (i *invalidateEntryRoot) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if name != "child" {
		return nil, syscall.ENOENT
	}
	i.lookup.Inc()
	i.t.Logf("Lookup called, #%d", i.lookup.Count())
	return fstestutil.File{}, nil
}

func TestInvalidateEntry(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &invalidateEntryRoot{
		t: t,
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{a}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := statHelper.Spawn(ctx, t)
	defer control.Close()

	for i := 0; i < 10; i++ {
		var got statResult
		if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
			t.Fatalf("calling helper: %v", err)
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
		var got statResult
		if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
			t.Fatalf("calling helper: %v", err)
		}
	}
	if g, e := a.lookup.Count(), uint32(2); g != e {
		t.Errorf("wrong Lookup call count: %d != %d", g, e)
	}
}

type cachedFile struct {
}

var _ fs.Node = cachedFile{}

func (f cachedFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o666
	// FreeBSD won't issue reads if the file is empty.
	a.Size = 4096
	return nil
}

var _ fs.NodeOpener = cachedFile{}

func (f cachedFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	resp.Flags |= fuse.OpenKeepCache
	return f, nil
}

type readErrRequest struct {
	Path      string
	WantErrno syscall.Errno
}

func doReadErr(ctx context.Context, req readErrRequest) (*struct{}, error) {
	f, err := os.Open(req.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, 4096)
	if _, err := f.Read(data); !errors.Is(err, req.WantErrno) {
		return nil, fmt.Errorf("wrong error: %v", err)
	}
	return &struct{}{}, nil
}

var readErrHelper = helpers.Register("readErr", httpjson.ServePOST(doReadErr))

func TestNotifyStore(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child := cachedFile{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": child}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readHelper.Spawn(ctx, t)
	defer control.Close()

	// prove that read doesn't work, and make sure node is cached
	{
		control := readErrHelper.Spawn(ctx, t)
		defer control.Close()
		req := readErrRequest{
			Path:      mnt.Dir + "/child",
			WantErrno: syscall.EOPNOTSUPP,
		}
		var nothing struct{}
		if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
			t.Fatalf("calling helper: %v", err)
		}
	}

	greeting := strings.Repeat("testing store\n", 500)
	if l := len(greeting); l < syscall.Getpagesize() {
		t.Fatalf("must fill at least one page to avoid second Read: len=%d", l)
	}
	t.Logf("storing...")
	if err := mnt.Server.NotifyStore(child, 0, []byte(greeting)); err != nil {
		if runtime.GOOS == "freebsd" && errors.Is(err, syscall.ENOSYS) {
			t.Skip("FreeBSD does not support NotifyStore")
		}
		t.Fatalf("store error: %v", err)
	}
	if runtime.GOOS == "freebsd" {
		t.Errorf("FreeBSD started supporting NotifyStore, update code")
	}

	var got readResult
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	// we have just read data without implementing Read!

	// doRead caps result at 4 kiB
	if g, e := string(got.Data), greeting[:4096]; g != e {
		t.Errorf("readAll = %q, want %q", g, e)
	}
}

func TestNotifyRetrieve(t *testing.T) {
	// This test may see false positive failures when run under
	// extreme memory pressure.
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child := readAll{}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": child}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readHelper.Spawn(ctx, t)
	defer control.Close()

	// read to fill page cache
	var got readResult
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	t.Logf("retrieving...")
	data, err := mnt.Server.NotifyRetrieve(child, 0, 5)
	if err != nil {
		if runtime.GOOS == "freebsd" && errors.Is(err, syscall.ENOSYS) {
			t.Skip("FreeBSD does not support NotifyRetrieve")
		}
		t.Fatalf("retrieve error: %v", err)
	}
	if runtime.GOOS == "freebsd" {
		t.Errorf("FreeBSD started supporting NotifyRetrieve, update code")
	}

	if g, e := string(data), hi[:5]; g != e {
		t.Errorf("retrieve = %q, want %q", g, e)
	}
}

type contextFile struct {
	fstestutil.File
}

var contextFileSentinel int

func (contextFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	v := ctx.Value(&contextFileSentinel)
	if v == nil {
		return nil, syscall.ESTALE
	}
	data, ok := v.(string)
	if !ok {
		return nil, syscall.EIO
	}
	resp.Flags |= fuse.OpenDirectIO
	return fs.DataHandle([]byte(data)), nil
}

func TestContext(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	control := readHelper.Spawn(ctx, t)
	defer control.Close()

	var got readResult
	if err := control.JSON("/").Call(ctx, mnt.Dir+"/child", &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := string(got.Data), input; g != e {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t,
		fstestutil.SimpleFS{&fstestutil.ChildMap{"child": goexitFile{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := openErrHelper.Spawn(ctx, t)
	defer control.Close()

	req := openRequest{
		Path:      mnt.Dir + "/child",
		Flags:     os.O_RDONLY,
		Perm:      0,
		WantErrno: syscall.EIO,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}

// Test Poll: NodePoller and HandlePoller

// pollDelayRead is a HandleReader that only lets a read succeed after
// one round of polling.
type pollDelayRead struct {
	t      testing.TB
	server *fs.Server

	mu   sync.Mutex
	seen []string

	wakeup atomic.Value
	ready  uint64
}

// Can be used as either Handle or Node. If these interfaces diverge,
// change this to a common core with two wrappers.
var _ fs.HandlePoller = (*pollDelayRead)(nil)
var _ fs.NodePoller = (*pollDelayRead)(nil)

func (r *pollDelayRead) saw(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.t.Logf("saw %s", s)
	r.seen = append(r.seen, s)
}

func (n *pollDelayRead) Poll(ctx context.Context, req *fuse.PollRequest, resp *fuse.PollResponse) error {
	if w, ok := req.Wakeup(); ok {
		n.wakeup.Store(w)
	}
	resp.REvents = fuse.PollOut
	if atomic.LoadUint64(&n.ready) == 1 {
		resp.REvents |= fuse.PollIn
		return nil
	}
	return nil
}

func (n *pollDelayRead) doWakeup() {
	n.saw("wakeup")
	atomic.StoreUint64(&n.ready, 1)
	if w, ok := n.wakeup.Load().(fuse.PollWakeup); ok {
		if err := n.server.NotifyPollWakeup(w); err != nil {
			n.t.Errorf("wakeup error: %v", err)
		}
	}
}

var _ fs.HandleReader = (*pollDelayRead)(nil)

func (n *pollDelayRead) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	if req.FileFlags&fuse.OpenNonblock == 0 {
		n.t.Errorf("expected a non-blocking read")
		return syscall.ENAMETOOLONG
	}
	if atomic.LoadUint64(&n.ready) == 0 {
		n.saw("read-eagain")
		time.AfterFunc(1*time.Millisecond, n.doWakeup)
		return syscall.EAGAIN
	}
	n.saw("read-ready")
	fuseutil.HandleRead(req, resp, []byte(hi))
	return nil
}

// Test NodePoller

type readPolledNode struct {
	pollDelayRead
}

var _ fs.Node = (*readPolledNode)(nil)

func (readPolledNode) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o666
	a.Size = uint64(len(hi))
	return nil
}

func TestReadPollNode(t *testing.T) {
	if runtime.GOOS == "freebsd" {
		t.Skip("no poll on FreeBSD")
	}
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child := &readPolledNode{
		pollDelayRead: pollDelayRead{
			t: t,
		},
	}
	filesys := fstestutil.SimpleFS{&fstestutil.ChildMap{"child": child}}
	setup := func(mnt *fstestutil.Mount) fs.FS {
		child.server = mnt.Server
		return filesys
	}
	mnt, err := fstestutil.MountedFuncT(t, setup, nil)
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
	if g, e := strings.Join(child.seen, " "), "read-eagain wakeup read-ready"; g != e {
		t.Errorf("wrong events: %q != %q", g, e)
	}
}

// Test HandlePoller

type readPolledNodeWithHandle struct {
	handle pollDelayRead
}

var _ fs.Node = (*readPolledNodeWithHandle)(nil)

func (readPolledNodeWithHandle) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o666
	a.Size = uint64(len(hi))
	return nil
}

var _ fs.NodeOpener = (*readPolledNodeWithHandle)(nil)

func (f *readPolledNodeWithHandle) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	return &f.handle, nil
}

func TestReadPollHandle(t *testing.T) {
	if runtime.GOOS == "freebsd" {
		t.Skip("no poll on FreeBSD")
	}
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child := &readPolledNodeWithHandle{
		handle: pollDelayRead{
			t: t,
		},
	}
	filesys := fstestutil.SimpleFS{&fstestutil.ChildMap{"child": child}}
	setup := func(mnt *fstestutil.Mount) fs.FS {
		child.handle.server = mnt.Server
		return filesys
	}
	mnt, err := fstestutil.MountedFuncT(t, setup, nil)
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
	if g, e := strings.Join(child.handle.seen, " "), "read-eagain wakeup read-ready"; g != e {
		t.Errorf("wrong events: %q != %q", g, e)
	}
}

// Test flock

// Go syscall & golang.org/x/sys/unix do a horrible thing where they
// muddle the difference between fcntl and flock by naming the syscall
// "FcntlFlock" and the result type "Flock_t". Make no mistake that is
// fcntl and has nothing to do with flock.

type lockFile struct {
	fstestutil.File

	lock    record.RequestRecorder
	unlock  record.RequestRecorder
	release record.ReleaseWaiter
	flush   record.RequestRecorder
}

var _ fs.NodeOpener = (*lockFile)(nil)

func (f *lockFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	h := &lockHandle{
		f: f,
	}
	return h, nil
}

type lockHandle struct {
	f *lockFile
}

var _ fs.Handle = (*lockHandle)(nil)

var _ fs.HandleLocker = (*lockHandle)(nil)

func (h *lockHandle) Lock(ctx context.Context, req *fuse.LockRequest) error {
	tmp := *req
	h.f.lock.RecordRequest(&tmp)
	return nil
}

func (h *lockHandle) LockWait(ctx context.Context, req *fuse.LockWaitRequest) error {
	tmp := *req
	h.f.lock.RecordRequest(&tmp)
	return nil
}

func (h *lockHandle) Unlock(ctx context.Context, req *fuse.UnlockRequest) error {
	tmp := *req
	h.f.unlock.RecordRequest(&tmp)
	return nil
}

func (h *lockHandle) QueryLock(ctx context.Context, req *fuse.QueryLockRequest, resp *fuse.QueryLockResponse) error {
	return nil
}

var _ fs.HandleFlockLocker = (*lockHandle)(nil)

var _ fs.HandleReleaser = (*lockHandle)(nil)

func (h *lockHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	return h.f.release.Release(ctx, req)
}

var _ fs.HandlePOSIXLocker = (*lockHandle)(nil)

var _ fs.HandleFlusher = (*lockHandle)(nil)

func (h *lockHandle) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	tmp := *req
	h.f.flush.RecordRequest(&tmp)
	return nil
}

type lockHelp struct {
	lockFn   func(fd uintptr, req *lockReq) error
	unlockFn func(fd uintptr, req *lockReq) error
	queryFn  func(fd uintptr, lk *unix.Flock_t) error

	mu   sync.Mutex
	file *os.File
}

func (lh *lockHelp) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/lock":
		httpjson.ServePOST(lh.doLock).ServeHTTP(w, req)
	case "/unlock":
		httpjson.ServePOST(lh.doUnlock).ServeHTTP(w, req)
	case "/close":
		httpjson.ServePOST(lh.doClose).ServeHTTP(w, req)
	case "/query":
		httpjson.ServePOST(lh.doQuery).ServeHTTP(w, req)
	default:
		http.NotFound(w, req)
	}
}

type lockReq struct {
	Path  string
	Wait  bool
	Start int64
	Len   int64
}

func (lh *lockHelp) doLock(ctx context.Context, req *lockReq) (*struct{}, error) {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	f, err := os.OpenFile(req.Path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open: %v", err)
	}
	lh.file = f
	c, err := lh.file.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("syscallconn: %v", err)
	}
	var outerr error
	lockFn := func(fd uintptr) {
		outerr = lh.lockFn(fd, req)
	}
	if err := c.Control(lockFn); err != nil {
		return nil, fmt.Errorf("error calling lock: %v", err)
	}
	if err := outerr; err != nil {
		return nil, fmt.Errorf("lock error: %v", err)
	}
	return &struct{}{}, nil
}

func (lh *lockHelp) doUnlock(ctx context.Context, req *lockReq) (*struct{}, error) {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	if lh.file == nil {
		return nil, errors.New("file not open")
	}
	c, err := lh.file.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("syscallconn: %v", err)
	}
	var outerr error
	unlockFn := func(fd uintptr) {
		outerr = lh.unlockFn(fd, req)
	}
	if err := c.Control(unlockFn); err != nil {
		return nil, fmt.Errorf("error calling unlock: %v", err)
	}
	if err := outerr; err != nil {
		return nil, fmt.Errorf("unlock error: %v", err)
	}
	return &struct{}{}, nil
}

func (lh *lockHelp) doClose(ctx context.Context, _ struct{}) (*struct{}, error) {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	if err := lh.file.Close(); err != nil {
		return nil, fmt.Errorf("Close: %v", err)
	}
	return &struct{}{}, nil
}

func (lh *lockHelp) doQuery(ctx context.Context, req *lockReq) (*unix.Flock_t, error) {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	if lh.file == nil {
		return nil, errors.New("file not open")
	}
	c, err := lh.file.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("syscallconn: %v", err)
	}
	var outerr error
	lk := unix.Flock_t{
		Type:   unix.F_WRLCK,
		Whence: int16(io.SeekStart),
		Start:  req.Start,
		Len:    req.Len,
	}
	queryFn := func(fd uintptr) {
		outerr = lh.queryFn(fd, &lk)
	}
	if err := c.Control(queryFn); err != nil {
		return nil, fmt.Errorf("error calling getlk: %v", err)
	}
	if err := outerr; err != nil {
		return nil, fmt.Errorf("query lock error: %v", err)
	}
	return &lk, nil
}

var lockFlockHelper = helpers.Register("lock-flock", &lockHelp{
	lockFn: func(fd uintptr, req *lockReq) error {
		flags := unix.LOCK_EX
		if !req.Wait {
			flags |= unix.LOCK_NB
		}
		return unix.Flock(int(fd), flags)
	},
	unlockFn: func(fd uintptr, req *lockReq) error {
		return unix.Flock(int(fd), unix.LOCK_UN)
	},
	queryFn: func(fd uintptr, lk *unix.Flock_t) error {
		return errors.New("no query in flock api")
	},
})

var lockPOSIXHelper = helpers.Register("lock-posix", &lockHelp{
	lockFn: func(fd uintptr, req *lockReq) error {
		lk := unix.Flock_t{
			Type:   unix.F_WRLCK,
			Whence: int16(io.SeekStart),
			Start:  req.Start,
			Len:    req.Len,
		}
		cmd := unix.F_SETLK
		if req.Wait {
			cmd = unix.F_SETLKW
		}
		//		return unix.FcntlFlock(fd, cmd, &lk)
		err := unix.FcntlFlock(fd, cmd, &lk)
		log.Printf("WTF L %d %v %#v: %v", fd, cmd, lk, err)
		return err
	},
	unlockFn: func(fd uintptr, req *lockReq) error {
		lk := unix.Flock_t{
			Type:   unix.F_UNLCK,
			Whence: int16(io.SeekStart),
			Start:  req.Start,
			Len:    req.Len,
		}
		cmd := unix.F_SETLK
		//		return unix.FcntlFlock(fd, cmd, &lk)
		err := unix.FcntlFlock(fd, cmd, &lk)
		log.Printf("WTF U %d %v %#v: %v", fd, cmd, lk, err)
		return err
	},
	queryFn: func(fd uintptr, lk *unix.Flock_t) error {
		cmd := unix.F_GETLK
		return unix.FcntlFlock(fd, cmd, lk)
	},
})

// ugly kludge to have platform-specific subtests. filled in
// elsewhere, when on linux.
var lockOFDHelper *spawntest.Helper

type lockTest struct {
	*testing.T
	ctx     context.Context
	kind    string
	child   *lockFile
	mnt     *fstestutil.Mount
	control *spawntest.Control
}

func (t *lockTest) recordedLockRequest() (_ *fuse.LockRequest, waited bool) {
	switch req := t.child.lock.Recorded().(type) {
	case *fuse.LockRequest:
		return req, false
	case *fuse.LockWaitRequest:
		return (*fuse.LockRequest)(req), true
	default:
		t.Fatalf("bad lock request: %#v", req)
	}
	panic("not reached")
}

func (t *lockTest) callLock(req *lockReq) {
	t.Logf("calling lock")
	if req.Path == "" {
		req.Path = "child"
	}
	req.Path = filepath.Join(t.mnt.Dir, req.Path)
	var nothing struct{}
	if err := t.control.JSON("/lock").Call(t.ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	want := &fuse.LockRequest{
		LockFlags: 0,
		Lock: fuse.FileLock{
			Start: uint64(req.Start),
			End:   uint64(req.Start) + uint64(req.Len) - 1,
			Type:  fuse.LockWrite,
			PID:   0,
		},
	}
	if t.kind == "flock" {
		want.LockFlags |= fuse.LockFlock
		want.Lock.Start = 0
		want.Lock.End = 0x7fff_ffff_ffff_ffff
	}
	got, waited := t.recordedLockRequest()
	if g, e := waited, req.Wait; g != e {
		t.Errorf("lock non-blocking field is bad: %v != %v", g, e)
	}
	// dynamic values that are too hard to control
	if got.Handle == 0 {
		t.Errorf("got LockRequest with no Handle")
	}
	want.Handle = got.Handle
	if got.LockOwner == 0 {
		t.Errorf("got LockRequest with no LockOwner")
	}
	want.LockOwner = got.LockOwner
	if got.Lock.PID == 0 {
		t.Errorf("got LockRequest with no PID")
	}
	want.Lock.PID = got.Lock.PID
	if g, e := got, want; *g != *e {
		t.Errorf("lock bad request\ngot\t%v\nwant\t%v", g, e)
	}
}

func (t *lockTest) callUnlock(req *lockReq) {
	t.Logf("calling unlock")
	var nothing struct{}
	if err := t.control.JSON("/unlock").Call(t.ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	lockReq, _ := t.recordedLockRequest()
	t.Logf("previous lock request: %v", lockReq)
	want := &fuse.UnlockRequest{
		LockOwner: lockReq.LockOwner,
		LockFlags: 0,
		Lock: fuse.FileLock{
			Start: uint64(req.Start),
			End:   uint64(req.Start) + uint64(req.Len) - 1,
			Type:  fuse.LockUnlock,
			PID:   0,
		},
	}
	if t.kind == "flock" {
		want.LockFlags |= fuse.LockFlock
		want.Lock.Start = 0
		want.Lock.End = 0x7fff_ffff_ffff_ffff
	}
	got := t.child.unlock.Recorded().(*fuse.UnlockRequest)
	// dynamic values that are too hard to control
	if got.Handle == 0 {
		t.Errorf("got UnlockRequest with no Handle")
	}
	want.Handle = got.Handle
	if g, e := got, want; *g != *e {
		t.Errorf("unlock bad request\ngot\t%v\nwant\t%v", g, e)
	}
}

func (t *lockTest) callCloseToUnlock() {
	t.Logf("calling close with automatic unlock")
	var nothing struct{}
	if err := t.control.JSON("/close").Call(t.ctx, struct{}{}, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}

	if t.kind == "posix" {
		lockReq, _ := t.recordedLockRequest()
		want := &fuse.FlushRequest{
			LockOwner: lockReq.LockOwner,
		}
		got := t.child.flush.Recorded().(*fuse.FlushRequest)
		// dynamic values that are too hard to control
		if got.Handle == 0 {
			t.Errorf("got FlushRequest with no Handle")
		}
		want.Handle = got.Handle
		if g, e := got, want; *g != *e {
			t.Errorf("close to unlock bad flush request\ngot\t%v\nwant\t%v", g, e)
		}
	}

	if t.kind == "flock" || t.kind == "ofd" {
		lockReq, _ := t.recordedLockRequest()
		want := &fuse.ReleaseRequest{
			Flags:     fuse.OpenReadWrite | fuse.OpenNonblock,
			LockOwner: lockReq.LockOwner,
		}
		if t.kind == "ofd" {
			// TODO linux kernel ofd FUSE support is very partial;
			// disable parts that don't work
			want.LockOwner = 0
		}
		if t.kind == "flock" {
			want.ReleaseFlags |= fuse.ReleaseFlockUnlock
		}
		got, ok := t.child.release.WaitForRelease(1 * time.Second)
		if !ok {
			t.Fatalf("Close did not Release in time")
		}
		// dynamic values that are too hard to control
		if got.Handle == 0 {
			t.Errorf("got ReleaseRequest with no Handle")
		}
		want.Handle = got.Handle
		if g, e := got, want; *g != *e {
			t.Errorf("bad release:\ngot\t%v\nwant\t%v", g, e)
		}
	}
}

func (t *lockTest) callQueryLock(req *lockReq) *unix.Flock_t {
	t.Logf("calling queryLock")
	var resp unix.Flock_t
	if err := t.control.JSON("/query").Call(t.ctx, req, &resp); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	return &resp
}

type lockFamily struct {
	name         string
	mountOptions []fuse.MountOption
	helper       *spawntest.Helper
}

func (family lockFamily) run(t *testing.T, name string, fn func(t *lockTest)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		maybeParallel(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		child := &lockFile{}
		mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{"child": child}}, nil, family.mountOptions...)
		if err != nil {
			t.Fatal(err)
		}
		defer mnt.Close()
		control := family.helper.Spawn(ctx, t)
		defer control.Close()
		lt := &lockTest{
			T:       t,
			ctx:     ctx,
			kind:    family.name,
			child:   child,
			mnt:     mnt,
			control: control,
		}
		fn(lt)
	})
}

func TestLocking(t *testing.T) {
	if runtime.GOOS == "freebsd" {
		// Non-exhaustive list of issues encountered, too many to add
		// workarounds or kludge tests:
		//
		//     - flock is not implemented
		//     - F_SETLKW comes through as non-blocking
		//     - fcntl F_UNLCK calls give EINVAL for some reason
		//     - LockRequest.LockOwner == 0
		//     - LockRequest.Lock.PID == 0, while Linux fills it
		t.Skip("FreeBSD locking support does not work")
	}

	t.Run("Flock", func(t *testing.T) {
		run := lockFamily{
			name:         "flock",
			mountOptions: []fuse.MountOption{fuse.LockingFlock()},
			helper:       lockFlockHelper,
		}.run
		run(t, "Nonblock", func(t *lockTest) {
			t.callLock(&lockReq{})
			t.callUnlock(&lockReq{})
		})
		run(t, "Wait", func(t *lockTest) {
			t.callLock(&lockReq{
				Wait: true,
			})
			t.callUnlock(&lockReq{})
		})
		run(t, "CloseUnlocks", func(t *lockTest) {
			t.callLock(&lockReq{})
			t.callCloseToUnlock()
		})
	})

	t.Run("POSIX", func(t *testing.T) {
		run := lockFamily{
			name:         "posix",
			mountOptions: []fuse.MountOption{fuse.LockingPOSIX()},
			helper:       lockPOSIXHelper,
		}.run
		run(t, "Nonblock", func(t *lockTest) {
			lr := &lockReq{
				Start: 42,
				Len:   13,
			}
			t.callLock(lr)
			t.callUnlock(lr)
		})
		run(t, "Wait", func(t *lockTest) {
			lr := &lockReq{
				Wait:  true,
				Start: 42,
				Len:   13,
			}
			t.callLock(lr)
			t.callUnlock(lr)
		})
		run(t, "CloseUnlocks", func(t *lockTest) {
			t.callLock(&lockReq{
				Wait:  true,
				Start: 42,
				Len:   13,
			})
			t.callCloseToUnlock()
		})
		run(t, "QueryLock", func(t *lockTest) {
			t.callLock(&lockReq{
				Wait:  true,
				Start: 42,
				Len:   13,
			})
			got := t.callQueryLock(&lockReq{
				Start: 40,
				Len:   10,
			})
			want := &unix.Flock_t{
				Type:   unix.F_UNLCK,
				Whence: io.SeekStart,
				Start:  40,
				Len:    10,
				Pid:    0,
			}
			if g, e := got, want; *g != *e {
				t.Errorf("bad query lock\ngot\t%#v\nwant\t%#v", g, e)
			}
		})
	})

	t.Run("OpenFileDescription", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("Open File Descriptor locks are Linux-only")
		}
		run := lockFamily{
			name:         "ofd",
			mountOptions: []fuse.MountOption{fuse.LockingPOSIX()},
			helper:       lockOFDHelper,
		}.run
		run(t, "Nonblock", func(t *lockTest) {
			lr := &lockReq{
				Start: 42,
				Len:   13,
			}
			t.callLock(lr)
			t.callUnlock(lr)
		})
		run(t, "Wait", func(t *lockTest) {
			lr := &lockReq{
				Wait:  true,
				Start: 42,
				Len:   13,
			}
			t.callLock(lr)
			t.callUnlock(lr)
		})
		run(t, "CloseUnlocks", func(t *lockTest) {
			t.callLock(&lockReq{
				Wait:  true,
				Start: 42,
				Len:   13,
			})
			t.callCloseToUnlock()
		})
		run(t, "QueryLock", func(t *lockTest) {
			t.callLock(&lockReq{
				Wait:  true,
				Start: 42,
				Len:   13,
			})
			got := t.callQueryLock(&lockReq{
				Start: 40,
				Len:   10,
			})
			want := &unix.Flock_t{
				Type:   unix.F_UNLCK,
				Whence: io.SeekStart,
				Start:  40,
				Len:    10,
				Pid:    0,
			}
			if g, e := got, want; *g != *e {
				t.Errorf("bad query lock\ngot\t%#v\nwant\t%#v", g, e)
			}
		})
	})

}
