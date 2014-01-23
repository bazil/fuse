package fs

import (
	"flag"
	"os"
	"testing"
)

import (
	"bazil.org/fuse"
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
