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

// All requests read from the kernel, without data, are shorter than
// this.
var maxRequestSize = syscall.Getpagesize()
var bufSize = uintptr(maxRequestSize + maxWrite)

// Our allocator requests this much. This is ~50MB
var memSize = 3 * bufSize

func MakeAlloc() *Allocator {
	return &Allocator{buf: make([]byte, memSize)}
}

func (a *Allocator) newRequest(c *Conn) *RequestScope {
	a.reset()

	scope := requestScope(a)
	scope.alloc = a
	scope.conn = c
	return scope
}

func requestScope(a *Allocator) *RequestScope {
	return (*RequestScope)(a.allocPointer(requestScopeSize))
}

type Allocator struct {
	buf  []byte
	next int
}

func (a *Allocator) alloc(size uintptr) []byte {
	s := int(size)
	if a.next+s > cap(a.buf) {
		panic(fmt.Sprintf("Not enough capacity: %v + %v > %v", a.next, size, cap(a.buf)))
	}
	r := a.buf[a.next : a.next+s]
	a.next += s
	return r
}

func (a *Allocator) allocPointer(size uintptr) unsafe.Pointer {
	return unsafe.Pointer(&a.alloc(size)[0])
}

func (a *Allocator) free(size int) {
	a.next -= size
	if a.next < 0 {
		panic(fmt.Sprintf("allocator.next below 0: %v %v", a.next, size))
	}
}

func (a *Allocator) reset() {
	b := a.buf[0:a.next]
	for idx := range b {
		b[idx] = 0
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
