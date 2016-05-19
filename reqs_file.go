package fuse

import (
	"fmt"
	"unsafe"
)

// Requests that operate on a file: open, read, release
type OpenRequest struct {
	openIn
}

const openRequestSize = unsafe.Sizeof(OpenRequest{})

type OpenResponse struct {
	openOut
}

func (r *OpenResponse) Handle(handle HandleID) {
	r.fh = uint64(handle)
}

func (r *OpenResponse) Flags(flags OpenResponseFlags) {
	r.openFlags = uint32(flags)
}

const openResponseSize = unsafe.Sizeof(OpenResponse{})

func openResponse(a *Allocator) *OpenResponse {
	return (*OpenResponse)(a.allocPointer(openResponseSize))
}

func (r *OpenResponse) Respond(s *RequestScope) {
	d := (*[openResponseSize]byte)(unsafe.Pointer(r))
	s.respond(d[:])
}

func parseOpen(b []byte, alloc *Allocator) (*OpenRequest, *OpenResponse, error) {
	if len(b) != int(openRequestSize) {
		return nil, nil, corrupt(b, openRequestSize)
	}
	req := (*OpenRequest)(unsafe.Pointer(&b[0]))
	resp := openResponse(alloc)
	resp.unique = req.unique
	return req, resp, nil
}

type ReadRequest struct {
	inHeader
	fh        uint64
	offset    uint64
	size      uint32
	readFlags uint32
	lockOwner uint64
	flags     uint32
	padding   uint32
}

func (r *ReadRequest) HandleID() HandleID {
	return HandleID(r.fh)
}

func (r *ReadRequest) Offset() int64 {
	return int64(r.offset)
}

// In the Fuse protocol, we respond to reads by sending the header and then the data.
// This means we will only send outHeader plus the first n bytes of data.
// dataRef is a slice that points into data only to store how many bytes we use.
type ReadResponse struct {
	outHeader
	data    [maxReadSize]byte
	dataRef []byte
}

func (r *ReadResponse) Data() []byte {
	return r.dataRef
}

func (r *ReadResponse) Size(size int) {
	if size < len(r.dataRef) {
		r.dataRef = r.dataRef[0:size]
	}
}

const readRequestSize = unsafe.Sizeof(ReadRequest{})

const readResponseBaseSize = unsafe.Offsetof(ReadResponse{}.data)

const readResponseSize = unsafe.Sizeof(ReadResponse{})

func readResponse(a *Allocator) *ReadResponse {
	return (*ReadResponse)(a.allocPointer(readResponseSize))
}

func (r *ReadResponse) Respond(s *RequestScope) {
	d := (*[readResponseSize]byte)(unsafe.Pointer(r))
	s.respond(d[:readResponseBaseSize+uintptr(len(r.dataRef))])
}

func readInSize(p Protocol) uintptr {
	if p.LT(Protocol{7, 9}) {
		return unsafe.Offsetof(ReadRequest{}.lockOwner)
	} else {
		return readRequestSize
	}
}

func parseRead(b []byte, alloc *Allocator, p Protocol) (*ReadRequest, *ReadResponse, error) {
	if len(b) != int(readInSize(p)) {
		return nil, nil, corrupt(b, readInSize(p))
	}
	req := (*ReadRequest)(unsafe.Pointer(&b[0]))
	if req.size > maxReadSize {
		return nil, nil, fmt.Errorf("Read of %v is larger than %v", req.size, maxReadSize)
	}
	resp := readResponse(alloc)
	resp.unique = req.unique
	resp.dataRef = resp.data[:req.size]
	return req, resp, nil
}

type ReleaseRequest struct {
	inHeader
	releaseIn
}

func (r *ReleaseRequest) HandleID() HandleID {
	return HandleID(r.fh)
}

type ReleaseResponse struct {
	outHeader
}

const releaseRequestSize = unsafe.Sizeof(ReleaseRequest{})

const releaseResponseSize = unsafe.Sizeof(ReleaseResponse{})

func releaseResponse(a *Allocator) *ReleaseResponse {
	return (*ReleaseResponse)(a.allocPointer(releaseResponseSize))
}

func (r *ReleaseResponse) Respond(s *RequestScope) {
	d := (*[releaseResponseSize]byte)(unsafe.Pointer(r))
	s.respond(d[:])
}

func parseRelease(b []byte, alloc *Allocator) (*ReleaseRequest, *ReleaseResponse, error) {
	if len(b) != int(releaseRequestSize) {
		return nil, nil, corrupt(b, releaseRequestSize)
	}
	req := (*ReleaseRequest)(unsafe.Pointer(&b[0]))
	resp := releaseResponse(alloc)
	resp.unique = req.unique
	return req, resp, nil
}
