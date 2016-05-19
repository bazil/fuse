package fuse

import (
	"fmt"
	"time"
	"unsafe"
)

// requests that operate on a non-open node: getattr, lookup, readlink

type GetattrRequest struct {
	inHeader
	in getattrIn
}

const getattrRequestSize = unsafe.Sizeof(GetattrRequest{})

const inHeaderSize = unsafe.Sizeof(inHeader{})

func (r *GetattrRequest) Node() NodeID {
	return NodeID(r.nodeid)
}

type GetattrResponse struct {
	outHeader
	out attrOut
}

const getattrResponseSize = unsafe.Sizeof(GetattrResponse{})

func attrOutSize(p Protocol) uintptr {
	switch {
	case p.LT(Protocol{7, 9}):
		return unsafe.Offsetof(attrOut{}.Attr) + unsafe.Offsetof(attrOut{}.Attr.Blksize)
	default:
		return unsafe.Sizeof(attrOut{})
	}
}

// the size of getattr response without blksize, which was added in fuse protocol 7.9
const legacyGetattrResponseSize = unsafe.Offsetof(GetattrResponse{}.out) + unsafe.Offsetof(attrOut{}.Attr) + unsafe.Offsetof(attr{}.Blksize)

func (r *GetattrResponse) Respond(s *RequestScope) {
	var d []byte
	if s.conn.proto.LT(Protocol{7, 9}) {
		d = (*[legacyGetattrResponseSize]byte)(unsafe.Pointer(r))[:]
	} else {
		d = (*[getattrResponseSize]byte)(unsafe.Pointer(r))[:]
	}
	s.respond(d)
}

func (r *GetattrResponse) Attr(attr Attr) {
	attr.attr(&r.out.Attr)
}

func getattrResponse(a *Allocator) *GetattrResponse {
	return (*GetattrResponse)(a.allocPointer(getattrResponseSize))
}

func parseGetattr(b []byte, alloc *Allocator, proto Protocol) (*GetattrRequest, *GetattrResponse, error) {

	if proto.LT(Protocol{7, 9}) {
		// Prior to fuse protocol 7.9, getattr only sent the header. So the size of
		// the buffer can't hold the entire GetattrRequest object.
		// alloc extra memory to reserve it (even though we're just going to leave it
		// zero'ed out)
		alloc.alloc(getattrRequestSize - inHeaderSize)
	} else {
		if len(b) < int(getattrRequestSize) {
			return nil, nil, corrupt(b, getattrRequestSize)
		}
	}
	req := (*GetattrRequest)(unsafe.Pointer(&b[0]))
	resp := getattrResponse(alloc)
	resp.unique = req.unique
	return req, resp, nil
}

type LookupRequest struct {
	inHeader
	name [maxWrite]byte
}

const lookupResponseSize = unsafe.Sizeof(LookupResponse{})

func lookupResponse(a *Allocator) *LookupResponse {
	return (*LookupResponse)(a.allocPointer(lookupResponseSize))
}

func (r *LookupResponse) NodeID(nodeID NodeID) {
	r.nodeid = uint64(nodeID)
}

func (r *LookupRequest) Name() string {
	// TODO(dbentley): we copy all the bytes to a new string.
	// We *could* make a function that returns a string that shares the byte buffer.
	// That would save us a copy, but be super dangerous.

	l := r.len - uint32(inHeaderSize) - 1 // an extra one to lop off null byte
	result := string(r.name[0:l])
	return result
}

func (r *LookupResponse) EntryValid(duration time.Duration) {
	r.entryValid = uint64(duration / time.Second)
	r.entryValidNsec = uint32(duration & time.Second / time.Nanosecond)
}

func (r *LookupResponse) Attr(attr Attr) {
	attr.attr(&r.attr)
}

// the size of lookup response without blksize, which was added in fuse protocol 7.9
const legacyLookupResponseSize = unsafe.Offsetof(LookupResponse{}.attr) + unsafe.Offsetof(attr{}.Blksize)

func (r *LookupResponse) Respond(s *RequestScope) {
	var d []byte
	if s.conn.proto.LT(Protocol{7, 9}) {
		d = (*[legacyLookupResponseSize]byte)(unsafe.Pointer(r))[:]
	} else {
		d = (*[lookupResponseSize]byte)(unsafe.Pointer(r))[:]
	}
	s.respond(d)
}

func parseLookup(b []byte, alloc *Allocator) (*LookupRequest, *LookupResponse, error) {
	req := (*LookupRequest)(unsafe.Pointer(&b[0]))
	resp := lookupResponse(alloc)
	resp.unique = req.unique
	l := req.len - uint32(inHeaderSize)
	if l == 0 {
		return nil, nil, fmt.Errorf("Malformed LookupRequest; no name")
	}
	if req.name[l-1] != '\x00' {
		return nil, nil, fmt.Errorf("Malformed LookupRequest: %v", req.name[l-1])
	}
	return req, resp, nil
}

type ReadlinkRequest struct {
	inHeader
}

type ReadlinkResponse struct {
	outHeader
	data    [maxReadSize]byte
	dataRef []byte
}

const readlinkRequestSize = unsafe.Sizeof(ReadlinkRequest{})

const readlinkResponseSize = unsafe.Sizeof(ReadlinkResponse{})

const readlinkResponseBaseSize = unsafe.Offsetof(ReadlinkResponse{}.data)

func readlinkResponse(a *Allocator) *ReadlinkResponse {
	return (*ReadlinkResponse)(a.allocPointer(readlinkResponseSize))
}

func (r *ReadlinkResponse) Respond(s *RequestScope) {
	d := (*[readlinkResponseSize]byte)(unsafe.Pointer(r))
	s.respond(d[:readlinkResponseBaseSize+uintptr(len(r.dataRef))])
}

func (r *ReadlinkResponse) Data(target string) {
	n := copy(r.dataRef, target)
	r.dataRef = r.dataRef[0:n]
}

func parseReadlink(b []byte, alloc *Allocator) (*ReadlinkRequest, *ReadlinkResponse, error) {
	if len(b) != int(readlinkRequestSize) {
		return nil, nil, corrupt(b, readlinkRequestSize)
	}
	req := (*ReadlinkRequest)(unsafe.Pointer(&b[0]))
	resp := readlinkResponse(alloc)
	resp.unique = req.unique
	resp.dataRef = resp.data[:]
	return req, resp, nil
}
