package fuse

import (
	"fmt"
	"unsafe"
)

// Reqs that operate on a directory: Opendir, Readdir, Releasedir

// NOTE: Open operations come in two flavors: file and directory.
// The data structures for the kernal interface are the same. So we use the same
// wire structures, and have different types around them that only wrap those.
type OpendirRequest struct {
	openIn
}

const opendirRequestSize = unsafe.Sizeof(OpendirRequest{})

type OpendirResponse struct {
	openOut
}

func (r *OpendirResponse) Handle(handle HandleID) {
	r.fh = uint64(handle)
}

func (r *OpendirResponse) Flags(flags OpenResponseFlags) {
	r.openFlags = uint32(flags)
}

const opendirResponseSize = unsafe.Sizeof(OpendirResponse{})

func opendirResponse(a *Allocator) *OpendirResponse {
	return (*OpendirResponse)(a.allocPointer(opendirResponseSize))
}

func (r *OpendirResponse) Respond(s *RequestScope) {
	d := (*[opendirResponseSize]byte)(unsafe.Pointer(r))
	s.respond(d[:])
}

func parseOpendir(b []byte, alloc *Allocator) (*OpendirRequest, *OpendirResponse, error) {
	if len(b) != int(opendirRequestSize) {
		return nil, nil, corrupt(b, opendirRequestSize)
	}
	req := (*OpendirRequest)(unsafe.Pointer(&b[0]))
	resp := opendirResponse(alloc)
	resp.unique = req.unique
	return req, resp, nil
}

type ReaddirRequest struct {
	inHeader
	fh        uint64
	offset    uint64
	size      uint32
	readFlags uint32
	lockOwner uint64
	flags     uint32
	padding   uint32
}

func (r *ReaddirRequest) HandleID() HandleID {
	return HandleID(r.fh)
}

func (r *ReaddirRequest) Offset() uint64 {
	return r.offset
}

func (r *ReaddirRequest) Size() uint32 {
	return r.size
}

// In the Fuse protocol, we respond to reads by sending the header and then the data.
// This means we will only send outHeader plus the first n bytes of data.
// dataRef is a slice that points into data only to store how many bytes we use.
type ReaddirResponse struct {
	dataRef []byte
	outHeader
	data [maxReadSize]byte
}

func (r *ReaddirResponse) Data(d []byte) {
	n := copy(r.dataRef, d)
	r.dataRef = r.dataRef[0:n]
}

const readdirRequestSize = unsafe.Sizeof(ReaddirRequest{})

const readdirResponseBaseSize = unsafe.Offsetof(ReaddirResponse{}.data)

const readdirResponseHeaderStart = unsafe.Offsetof(ReaddirResponse{}.outHeader)

const readdirResponseSize = unsafe.Sizeof(ReaddirResponse{})

func readdirResponse(a *Allocator, dataSize uint32) *ReaddirResponse {
	return (*ReaddirResponse)(a.allocPointer(readdirResponseBaseSize + uintptr(dataSize)))
}

func (r *ReaddirResponse) Respond(s *RequestScope) {
	d := (*[readdirResponseSize]byte)(unsafe.Pointer(r))
	s.respond(d[readdirResponseHeaderStart : readdirResponseBaseSize+uintptr(len(r.dataRef))])
}

func readdirInSize(p Protocol) uintptr {
	if p.LT(Protocol{7, 9}) {
		return unsafe.Offsetof(ReaddirRequest{}.lockOwner)
	} else {
		return readdirRequestSize
	}
}

func parseReaddir(b []byte, alloc *Allocator, p Protocol) (*ReaddirRequest, *ReaddirResponse, error) {
	if len(b) != int(readdirInSize(p)) {
		return nil, nil, corrupt(b, readdirInSize(p))
	}
	req := (*ReaddirRequest)(unsafe.Pointer(&b[0]))
	if req.size > maxReadSize {
		return nil, nil, fmt.Errorf("Readdir of %v is larger than %v", req.size, maxReadSize)
	}
	resp := readdirResponse(alloc, req.size)
	resp.unique = req.unique
	resp.dataRef = resp.data[:req.size]
	return req, resp, nil
}

type ReleasedirRequest struct {
	inHeader
	releaseIn
}

func (r *ReleasedirRequest) HandleID() HandleID {
	return HandleID(r.fh)
}

type ReleasedirResponse struct {
	outHeader
}

const releasedirRequestSize = unsafe.Sizeof(ReleasedirRequest{})

const releasedirResponseSize = unsafe.Sizeof(ReleasedirResponse{})

func releasedirResponse(a *Allocator) *ReleasedirResponse {
	return (*ReleasedirResponse)(a.allocPointer(releasedirResponseSize))
}

func (r *ReleasedirResponse) Respond(s *RequestScope) {
	d := (*[releasedirResponseSize]byte)(unsafe.Pointer(r))
	s.respond(d[:])
}

func parseReleasedir(b []byte, alloc *Allocator) (*ReleasedirRequest, *ReleasedirResponse, error) {
	if len(b) != int(releasedirRequestSize) {
		return nil, nil, corrupt(b, releasedirRequestSize)
	}
	req := (*ReleasedirRequest)(unsafe.Pointer(&b[0]))
	resp := releasedirResponse(alloc)
	resp.unique = req.unique
	return req, resp, nil
}
