package record

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"sync"
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

// Flushes notes whether a FUSE Flush call has been seen.
type Flushes struct {
	rec MarkRecorder
}

var _ = fs.HandleFlusher(&Flushes{})

func (r *Flushes) Flush(req *fuse.FlushRequest, intr fs.Intr) fuse.Error {
	r.rec.Mark()
	return nil
}

func (r *Flushes) RecordedFlush() bool {
	return r.rec.Recorded()
}

type Recorder struct {
	mu  sync.Mutex
	val interface{}
}

// Record that we've seen value. A nil value is indistinguishable from
// no value recorded.
func (r *Recorder) Record(value interface{}) {
	r.mu.Lock()
	r.val = value
	r.mu.Unlock()
}

func (r *Recorder) Recorded() interface{} {
	r.mu.Lock()
	val := r.val
	r.mu.Unlock()
	return val
}

type RequestRecorder struct {
	rec Recorder
}

// Record a fuse.Request, after zeroing header fields that are hard to
// reproduce.
//
// Make sure to record a copy, not the original request.
func (r *RequestRecorder) RecordRequest(req fuse.Request) {
	hdr := req.Hdr()
	*hdr = fuse.Header{}
	r.rec.Record(req)
}

func (r *RequestRecorder) Recorded() fuse.Request {
	val := r.rec.Recorded()
	if val == nil {
		return nil
	}
	return val.(fuse.Request)
}

// Mkdirs records a Mkdir request and its fields.
type Mkdirs struct {
	rec RequestRecorder
}

var _ = fs.NodeMkdirer(&Mkdirs{})

// Mkdir records the request and returns an error. Most callers should
// wrap this call in a function that returns a more useful result.
func (r *Mkdirs) Mkdir(req *fuse.MkdirRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	tmp := *req
	r.rec.RecordRequest(&tmp)
	return nil, fuse.EIO
}

// RecordedMkdir returns information about the Mkdir request.
// If no request was seen, returns a zero value.
func (r *Mkdirs) RecordedMkdir() fuse.MkdirRequest {
	val := r.rec.Recorded()
	if val == nil {
		return fuse.MkdirRequest{}
	}
	return *(val.(*fuse.MkdirRequest))
}

// Symlinks records a Symlink request and its fields.
type Symlinks struct {
	rec RequestRecorder
}

var _ = fs.NodeSymlinker(&Symlinks{})

// Symlink records the request and returns an error. Most callers should
// wrap this call in a function that returns a more useful result.
func (r *Symlinks) Symlink(req *fuse.SymlinkRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	tmp := *req
	r.rec.RecordRequest(&tmp)
	return nil, fuse.EIO
}

// RecordedSymlink returns information about the Symlink request.
// If no request was seen, returns a zero value.
func (r *Symlinks) RecordedSymlink() fuse.SymlinkRequest {
	val := r.rec.Recorded()
	if val == nil {
		return fuse.SymlinkRequest{}
	}
	return *(val.(*fuse.SymlinkRequest))
}

// Links records a Link request and its fields.
type Links struct {
	rec RequestRecorder
}

var _ = fs.NodeLinker(&Links{})

// Link records the request and returns an error. Most callers should
// wrap this call in a function that returns a more useful result.
func (r *Links) Link(req *fuse.LinkRequest, old fs.Node, intr fs.Intr) (fs.Node, fuse.Error) {
	tmp := *req
	r.rec.RecordRequest(&tmp)
	return nil, fuse.EIO
}

// RecordedLink returns information about the Link request.
// If no request was seen, returns a zero value.
func (r *Links) RecordedLink() fuse.LinkRequest {
	val := r.rec.Recorded()
	if val == nil {
		return fuse.LinkRequest{}
	}
	return *(val.(*fuse.LinkRequest))
}
