package fuse

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Maximum file write size we are prepared to receive from the kernel.
const maxWrite = 16 * 1024 * 1024

// TODO(dbentley): Why does this seem reasonable?
const maxReadSize = maxWrite

const maxFilenameLen = 1024 // Maximum filename length we support

// All requests read from the kernel, without data, are shorter than
// this.
var maxRequestSize = syscall.Getpagesize()
var bufSize = uintptr(maxRequestSize + maxWrite)

// Our allocator requests this much. This is ~50MB
var memSize = 3 * bufSize

func MakeAlloc() *Allocator {
	return &Allocator{buf: make([]byte, memSize)}
}

// Note, the allocator funcs take a zeroed flag as an optimization.
// Set zeroed to true if relying on struct/array default values when casting from bytes.
// Set to false if bytes are immediately overwritten by the caller making zeroing superfluous.

func (a *Allocator) newRequest(c *Conn, zeroed bool) *RequestScope {

	a.reset(zeroed)

	scope := requestScope(a, false)
	scope.alloc = a
	scope.conn = c
	scope.Req = nil
	scope.Resp = nil
	return scope
}

func requestScope(a *Allocator, zeroed bool) *RequestScope {
	return (*RequestScope)(a.allocPointer(requestScopeSize, zeroed))
}

type Allocator struct {
	buf  []byte
	next int
}

func (a *Allocator) alloc(size uintptr, zeroed bool) []byte {
	s := int(size)
	if a.next+s > cap(a.buf) {
		panic(fmt.Sprintf("Not enough capacity: %v + %v > %v", a.next, size, cap(a.buf)))
	}
	r := a.buf[a.next : a.next+s]
	a.next += s
	if zeroed {
		for ii := 0; ii < len(r); ii++ {
			r[ii] = 0
		}
	}
	return r
}

func (a *Allocator) allocPointer(size uintptr, zeroed bool) unsafe.Pointer {
	return unsafe.Pointer(&a.alloc(size, zeroed)[0])
}

func (a *Allocator) free(size int) {
	a.next -= size
	if a.next < 0 {
		panic(fmt.Sprintf("allocator.next below 0: %v %v", a.next, size))
	}
}

func (a *Allocator) reset(zeroed bool) {
	if zeroed {
		b := a.buf[0:a.next]
		for idx := range b {
			b[idx] = 0
		}
	}
	a.next = 0
}

type RequestScope struct {
	conn  *Conn
	alloc *Allocator
	Req   Request
	Resp  Response
}

const requestScopeSize = unsafe.Sizeof(RequestScope{})

func (s *RequestScope) Release() {
	// TODO(dbentley): here is where we could respond if we haven't responded yet
}

func (s *RequestScope) respond(data []byte) {
	// TODO(dbentley): here is where we could record that we've responded
	s.conn.Respond(data)
}
