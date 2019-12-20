package bench_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"testing"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
	"bazil.org/fuse/fs/fstestutil/spawntest/httpjson"
)

type dummyFile struct {
	fstestutil.File
}

type benchCreateDir struct {
	fstestutil.Dir
}

var _ fs.NodeCreater = (*benchCreateDir)(nil)

func (f *benchCreateDir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	child := &dummyFile{}
	return child, child, nil
}

type benchCreateHelp struct {
	mu    sync.Mutex
	n     int
	names []string
}

func (b *benchCreateHelp) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/init":
		httpjson.ServePOST(b.doInit).ServeHTTP(w, req)
	case "/bench":
		httpjson.ServePOST(b.doBench).ServeHTTP(w, req)
	default:
		http.NotFound(w, req)
	}
}

type benchCreateInitRequest struct {
	Dir string
	N   int
}

func (b *benchCreateHelp) doInit(ctx context.Context, req benchCreateInitRequest) (*struct{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// prepare file names to decrease test overhead
	names := make([]string, 0, req.N)
	for i := 0; i < req.N; i++ {
		// zero-padded so cost stays the same on every iteration
		names = append(names, req.Dir+"/"+fmt.Sprintf("%08x", i))
	}
	b.n = req.N
	b.names = names
	return &struct{}{}, nil
}

func (b *benchCreateHelp) doBench(ctx context.Context, _ struct{}) (*struct{}, error) {
	for i := 0; i < b.n; i++ {
		f, err := os.Create(b.names[i])
		if err != nil {
			log.Fatalf("Create: %v", err)
		}
		f.Close()
	}
	return &struct{}{}, nil
}

var benchCreateHelper = helpers.Register("benchCreate", &benchCreateHelp{})

func BenchmarkCreate(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &benchCreateDir{}
	mnt, err := fstestutil.MountedT(b, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer mnt.Close()
	control := benchCreateHelper.Spawn(ctx, b)
	defer control.Close()

	req := benchCreateInitRequest{
		Dir: mnt.Dir,
		N:   b.N,
	}
	var nothing struct{}
	if err := control.JSON("/init").Call(ctx, req, &nothing); err != nil {
		b.Fatalf("calling helper: %v", err)
	}
	b.ResetTimer()

	if err := control.JSON("/bench").Call(ctx, struct{}{}, &nothing); err != nil {
		b.Fatalf("calling helper: %v", err)
	}
}
