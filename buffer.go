package fuse

import (
	"bytes"
	"sync"
	"unsafe"
)

// buffer provides a mechanism for constructing a message from
// multiple segments.
type buffer []byte

const hdrSize = unsafe.Sizeof(outHeader{})

// alloc allocates size bytes and returns a pointer to the new
// segment.
func (w *buffer) alloc(size uintptr) unsafe.Pointer {
	s := int(size)
	if len(*w)+s > cap(*w) {
		old := *w
		*w = make([]byte, len(*w), 2*cap(*w)+s)
		copy(*w, old)
	}
	l := len(*w)
	*w = (*w)[:l+s]
	return unsafe.Pointer(&(*w)[l])
}

// reset clears out the contents of the buffer.
func (w *buffer) reset() {
	for i := range (*w)[:cap(*w)] {
		(*w)[i] = 0
	}
	*w = (*w)[:0]
}

func newBuffer(extra uintptr) buffer {
	buf := make(buffer, hdrSize, hdrSize+extra)
	return buf
}

// readBufPool is a pool of read request data
var readBufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, int(hdrSize)+maxWrite)
	},
}

func newStreamingBuffer() *bytes.Buffer {
	buf := bytes.NewBuffer(readBufPool.Get().([]byte))
	buf.Truncate(int(hdrSize))

	return buf
}

func returnBuffer(buf []byte) {
	readBufPool.Put(buf)
}
