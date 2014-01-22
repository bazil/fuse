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

// MarkRecorder records whether a thing has occurred.
type MarkRecorder struct {
	count uint32
}

func (r *MarkRecorder) Mark() {
	atomic.StoreUint32(&r.count, 1)
}

func (r *MarkRecorder) Recorded() bool {
	return atomic.LoadUint32(&r.count) > 0
}

// Fsyncs notes whether a FUSE Fsync call has been seen.
type Fsyncs struct {
	rec MarkRecorder
}

var _ = fs.NodeFsyncer(&Fsyncs{})

func (r *Fsyncs) Fsync(req *fuse.FsyncRequest, intr fs.Intr) fuse.Error {
	r.rec.Mark()
	return nil
}

func (r *Fsyncs) RecordedFsync() bool {
	return r.rec.Recorded()
}

// Setattrs notes whether a FUSE Setattr call has been seen.
type Setattrs struct {
	rec MarkRecorder
}

var _ = fs.NodeSetattrer(&Setattrs{})

func (r *Setattrs) Setattr(req *fuse.SetattrRequest, resp *fuse.SetattrResponse, intr fs.Intr) fuse.Error {
	r.rec.Mark()
	return nil
}

func (r *Setattrs) RecordedSetattr() bool {
	return r.rec.Recorded()
}
