package fuse

import (
	"fmt"
	"unsafe"
)

type Request interface {
}

type Response interface {
	RespondError(err error, s *RequestScope)
}

func (h *inHeader) NodeID() NodeID {
	return NodeID(h.nodeid)
}

func corrupt(b []byte, expected uintptr) error {
	return fmt.Errorf("Wrong number of bytes in msg. Got %v, expected %v", len(b), expected)
}

func (h *outHeader) RespondError(err error, s *RequestScope) {
	errno := DefaultErrno
	if ferr, ok := err.(ErrorNumber); ok {
		errno = ferr.Errno()
	}
	// FUSE uses negative errors!
	// TODO: File bug report against OSXFUSE: positive error causes kernel panic.
	h.error = -int32(errno)
	d := (*[outHeaderSize]byte)(unsafe.Pointer(h))
	s.respond(d[:])
}

const outHeaderSize = unsafe.Sizeof(outHeader{})

func (r *outHeader) respond(s *RequestScope) {
	d := (*[outHeaderSize]byte)(unsafe.Pointer(r))
	s.respond(d[:])
}

func ParseOpcode(data []byte) (OpCode, error) {
	if len(data) < int(inHeaderSize) {
		return OpInvalid, fmt.Errorf("fuse: read too short %d", len(data))
	}
	h := (*inHeader)(unsafe.Pointer(&data[0]))
	return OpCode(h.opcode), nil
}

// Below is the skeleton to handle a new kind of Fuse request.
// Uncomment and replace foo with your optype.

// type <Foo>Request struct {
// 	inHeader
// }

// type <Foo>Response struct {
// 	outHeader
// }

// const <foo>RequestSize = unsafe.Sizeof(<Foo>Request{})

// const <foo>ResponseSize = unsafe.Sizeof(<Foo>Response{})

// func <foo>Response(a *Allocator) *<Foo>Response {
// 	return (*<Foo>Response)(a.allocPointer(<foo>ResponseSize))
// }

// func (r *<Foo>Response) Respond(s *RequestScope) {
// 	d := (*[<foo>ResponseSize]byte)(unsafe.Pointer(r))
// 	s.respond(d[:])
// }

// func parse<Foo>(b []byte, alloc *Allocator) (*<Foo>Request, *<Foo>Response, error) {
// 	if len(b) != int(<foo>RequestSize) {
// 		return nil, nil, corrupt(b, <foo>RequestSize)
// 	}
// 	req := (*<Foo>Request)(unsafe.Pointer(&b[0]))
// 	resp := <foo>Response(alloc)
// 	resp.unique = req.unique
// 	return req, resp, nil
// }
