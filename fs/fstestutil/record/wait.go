package record

import (
	"context"
	"sync"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

// ReleaseWaiter notes whether a FUSE Release call has been seen.
//
// Releases are not guaranteed to happen synchronously with any client
// call, so they must be waited for.
type ReleaseWaiter struct {
	once sync.Once
	seen chan *fuse.ReleaseRequest
}

var _ fs.HandleReleaser = (*ReleaseWaiter)(nil)

func (r *ReleaseWaiter) init() {
	r.once.Do(func() {
		r.seen = make(chan *fuse.ReleaseRequest, 1)
	})
}

func (r *ReleaseWaiter) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	r.init()
	tmp := *req
	hdr := tmp.Hdr()
	*hdr = fuse.Header{}
	r.seen <- &tmp
	close(r.seen)
	return nil
}

// WaitForRelease waits for Release to be called.
//
// With zero duration, wait forever. Otherwise, timeout early
// in a more controlled way than `-test.timeout`.
//
// Returns a sanitized ReleaseRequest and whether a Release was seen.
// Always true if dur==0.
func (r *ReleaseWaiter) WaitForRelease(dur time.Duration) (*fuse.ReleaseRequest, bool) {
	r.init()
	var timeout <-chan time.Time
	if dur > 0 {
		timeout = time.After(dur)
	}
	select {
	case req := <-r.seen:
		return req, true
	case <-timeout:
		return nil, false
	}
}
