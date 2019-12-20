package fs_test

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"

	"bazil.org/fuse/fs/fstestutil"
	"golang.org/x/sys/unix"
)

type exchangeData struct {
	fstestutil.File
	// this struct cannot be zero size or multiple instances may look identical
	_ int
}

type exchangedataRequest struct {
	Path1     string
	Path2     string
	Options   int
	WantErrno syscall.Errno
}

func doExchange(ctx context.Context, req exchangedataRequest) (*struct{}, error) {
	if err := unix.Exchangedata(req.Path1, req.Path2, req.Options); !errors.Is(err, req.WantErrno) {
		return nil, fmt.Errorf("from error from exchangedata: %v", err)
	}
	return &struct{}{}, nil
}

func TestExchangeDataNotSupported(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{&fstestutil.ChildMap{
		"one": &exchangeData{},
		"two": &exchangeData{},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := linkHelper.Spawn(ctx, t)
	defer control.Close()

	req := exchangedataRequest{
		Path1:     mnt.Dir + "/one",
		Path2:     mnt.Dir + "/two",
		Options:   0,
		WantErrno: syscall.ENOTSUP,
	}
	var nothing struct{}
	if err := control.JSON("/").Call(ctx, req, &nothing); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
}
