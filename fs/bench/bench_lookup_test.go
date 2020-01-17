package bench_test

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
	"bazil.org/fuse/fs/fstestutil/spawntest/httpjson"
)

type benchLookupDir struct {
	fstestutil.Dir
}

var _ fs.NodeRequestLookuper = (*benchLookupDir)(nil)

func (f *benchLookupDir) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (fs.Node, error) {
	return nil, syscall.ENOENT
}

type benchLookupRequest struct {
	Path string
	N    int
}

func doBenchLookup(ctx context.Context, req benchLookupRequest) (*struct{}, error) {
	for i := 0; i < req.N; i++ {
		if _, err := os.Stat(req.Path); !os.IsNotExist(err) {
			return nil, fmt.Errorf("Stat: wrong error: %v", err)
		}
	}
	return &struct{}{}, nil
}

var benchLookupHelper = helpers.Register("benchLookup", httpjson.ServePOST(doBenchLookup))

func BenchmarkLookup(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &benchLookupDir{}
	mnt, err := fstestutil.MountedT(b, fstestutil.SimpleFS{f}, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer mnt.Close()
	control := benchLookupHelper.Spawn(ctx, b)
	defer control.Close()

	name := mnt.Dir + "/does-not-exist"
	req := benchLookupRequest{
		Path: name,
		N:    b.N,
	}
	var nothing struct{}
	b.ResetTimer()

	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		b.Fatalf("calling helper: %v", err)
	}
}
