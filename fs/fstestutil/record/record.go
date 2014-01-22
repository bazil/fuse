package record

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"sync/atomic"
)

// Writes gathers data from FUSE Write calls.
type Writes struct {
	buf Buffer
}

var _ = fs.HandleWriter(&Writes{})

func (w *Writes) Write(req *fuse.WriteRequest, resp *fuse.WriteResponse, intr fs.Intr) fuse.Error {
	n, err := w.buf.Write(req.Data)
	resp.Size = n
	if err != nil {
		// TODO hiding error
		return fuse.EIO
	}
	return nil
}

func (w *Writes) RecordedWriteData() []byte {
	return w.buf.Bytes()
}

// Fsyncs notes whether a FUSE Fsync call has been seen.
type Fsyncs struct {
	count uint32
}

var _ = fs.NodeFsyncer(&Fsyncs{})

func (r *Fsyncs) Fsync(req *fuse.FsyncRequest, intr fs.Intr) fuse.Error {
	atomic.StoreUint32(&r.count, 1)
	return nil
}

func (r *Fsyncs) RecordedFsync() bool {
	return atomic.LoadUint32(&r.count) > 0
}
