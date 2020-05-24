package bench_test

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"syscall"
	"testing"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
	"bazil.org/fuse/fs/fstestutil/spawntest"
	"bazil.org/fuse/fs/fstestutil/spawntest/httpjson"
)

type benchConfig struct {
	directIO bool
}

type benchFS struct {
	conf *benchConfig
}

var _ fs.FS = (*benchFS)(nil)

func (f *benchFS) Root() (fs.Node, error) {
	return benchDir{fs: f}, nil
}

type benchDir struct {
	fs *benchFS
}

var _ fs.Node = benchDir{}
var _ fs.NodeStringLookuper = benchDir{}
var _ fs.Handle = benchDir{}
var _ fs.HandleReadDirAller = benchDir{}

func (benchDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = 1
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d benchDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if name == "bench" {
		return benchFile{conf: d.fs.conf}, nil
	}
	return nil, syscall.ENOENT
}

func (benchDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	l := []fuse.Dirent{
		{Inode: 2, Name: "bench", Type: fuse.DT_File},
	}
	return l, nil
}

type benchFile struct {
	conf *benchConfig
}

var _ fs.Node = benchFile{}
var _ fs.NodeOpener = benchFile{}
var _ fs.NodeFsyncer = benchFile{}
var _ fs.Handle = benchFile{}
var _ fs.HandleReader = benchFile{}
var _ fs.HandleWriter = benchFile{}

func (benchFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = 2
	a.Mode = 0o644
	a.Size = 9999999999999999
	return nil
}

func (f benchFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	if f.conf.directIO {
		resp.Flags |= fuse.OpenDirectIO
	}
	// TODO configurable?
	resp.Flags |= fuse.OpenKeepCache
	return f, nil
}

func (benchFile) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	resp.Data = resp.Data[:cap(resp.Data)]
	return nil
}

func (benchFile) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	resp.Size = len(req.Data)
	return nil
}

func (benchFile) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	return nil
}

type benchReadWriteRequest struct {
	Path string
	Size int64
	N    int
}

func benchmark(helper *spawntest.Helper, conf *benchConfig, size int64) func(*testing.B) {
	fn := func(b *testing.B) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		filesys := &benchFS{
			conf: conf,
		}
		mnt, err := fstestutil.Mounted(filesys, nil,
			fuse.MaxReadahead(64*1024*1024),
			fuse.AsyncRead(),
			fuse.WritebackCache(),
		)
		if err != nil {
			b.Fatal(err)
		}
		defer mnt.Close()
		control := helper.Spawn(ctx, b)
		defer control.Close()

		name := mnt.Dir + "/bench"
		req := benchReadWriteRequest{
			Path: name,
			Size: size,
			N:    b.N,
		}
		var nothing struct{}
		b.SetBytes(size)
		b.ResetTimer()

		if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
			b.Fatalf("calling helper: %v", err)
		}
	}
	return fn
}

func benchmarkSizes(helper *spawntest.Helper, conf *benchConfig) func(*testing.B) {
	fn := func(b *testing.B) {
		b.Run("100", benchmark(helper, conf, 100))
		b.Run("10MB", benchmark(helper, conf, 10*1024*1024))
		b.Run("100MB", benchmark(helper, conf, 100*1024*1024))
	}
	return fn
}

type zero struct{}

func (zero) Read(p []byte) (n int, err error) {
	return len(p), nil
}

var Zero io.Reader = zero{}

func doBenchWrite(ctx context.Context, req benchReadWriteRequest) (*struct{}, error) {
	f, err := os.Create(req.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	for i := 0; i < req.N; i++ {
		if _, err := io.CopyN(f, Zero, req.Size); err != nil {
			return nil, err
		}
	}
	return &struct{}{}, nil
}

var benchWriteHelper = helpers.Register("benchWrite", httpjson.ServePOST(doBenchWrite))

func BenchmarkWrite(b *testing.B) {
	b.Run("pagecache", benchmarkSizes(benchWriteHelper, &benchConfig{}))
	b.Run("direct", benchmarkSizes(benchWriteHelper, &benchConfig{
		directIO: true,
	}))
}

func doBenchWriteSync(ctx context.Context, req benchReadWriteRequest) (*struct{}, error) {
	f, err := os.Create(req.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	for i := 0; i < req.N; i++ {
		if _, err := io.CopyN(f, Zero, req.Size); err != nil {
			return nil, err
		}
		if err := f.Sync(); err != nil {
			return nil, err
		}
	}
	return &struct{}{}, nil
}

var benchWriteSyncHelper = helpers.Register("benchWriteSync", httpjson.ServePOST(doBenchWriteSync))

func BenchmarkWriteSync(b *testing.B) {
	b.Run("pagecache", benchmarkSizes(benchWriteSyncHelper, &benchConfig{}))
	b.Run("direct", benchmarkSizes(benchWriteSyncHelper, &benchConfig{
		directIO: true,
	}))
}

func doBenchRead(ctx context.Context, req benchReadWriteRequest) (*struct{}, error) {
	f, err := os.Create(req.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	for i := 0; i < req.N; i++ {
		n, err := io.CopyN(ioutil.Discard, f, req.Size)
		if err != nil {
			return nil, err
		}
		if n != req.Size {
			return nil, fmt.Errorf("unexpected size: %d != %d", n, req.Size)
		}
	}
	return &struct{}{}, nil
}

var benchReadHelper = helpers.Register("benchRead", httpjson.ServePOST(doBenchRead))

func BenchmarkRead(b *testing.B) {
	b.Run("pagecache", benchmarkSizes(benchReadHelper, &benchConfig{}))
	b.Run("direct", benchmarkSizes(benchReadHelper, &benchConfig{
		directIO: true,
	}))
}
