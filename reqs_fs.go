package fuse

import (
	"unsafe"
)

// requests that operate on the fs. init, statfs, unsupported requests

const initInSize = unsafe.Sizeof(initIn{})

type initRequest struct {
	inHeader
	in initIn
}

const initRequestSize = unsafe.Sizeof(initRequest{})

func (r *initRequest) Kernel() Protocol {
	return Protocol{r.in.Major, r.in.Minor}
}

func (r *initRequest) MaxReadahead() uint32 {
	return r.in.MaxReadahead
}

func (r *initRequest) Flags() InitFlags {
	return InitFlags(r.in.Flags)
}

type initResponse struct {
	outHeader
	out initOut
}

const initResponseSize = unsafe.Sizeof(initResponse{})

func (r *initResponse) Library(p Protocol) {
	r.out.Major = p.Major
	r.out.Minor = p.Minor
}

func (r *initResponse) MaxReadahead(max uint32) {
	r.out.MaxReadahead = max
}

func (r *initResponse) Flags(flags InitFlags) {
	r.out.Flags = uint32(flags)
}

func (r *initResponse) MaxWrite(max uint32) {
	if max > maxWrite {
		r.out.MaxWrite = maxWrite
	} else {
		r.out.MaxWrite = max
	}
}

func newInitResponse(a *Allocator) *initResponse {
	return (*initResponse)(a.allocPointer(initResponseSize))
}

func (r *initResponse) Respond(s *RequestScope) {
	d := (*[initResponseSize]byte)(unsafe.Pointer(r))
	s.respond(d[:])
}

func parseInit(b []byte, alloc *Allocator) (*initRequest, *initResponse, error) {
	h := (*inHeader)(unsafe.Pointer(&b[0]))
	if len(b) != int(initRequestSize) {
		return nil, nil, corrupt(b, initRequestSize)
	}
	req := (*initRequest)(unsafe.Pointer(&b[0]))
	resp := newInitResponse(alloc)
	resp.unique = h.unique
	return req, resp, nil
}

func statfsResponse(a *Allocator) *StatfsResponse {
	return (*StatfsResponse)(a.allocPointer(statfsResponseSize))
}

func parseStatfs(b []byte, alloc *Allocator) (*StatfsRequest, *StatfsResponse, error) {
	h := (*inHeader)(unsafe.Pointer(&b[0]))
	if len(b) != int(statfsRequestSize) {
		return nil, nil, corrupt(b, statfsRequestSize)
	}
	req := (*StatfsRequest)(unsafe.Pointer(&b[0]))
	resp := statfsResponse(alloc)
	resp.unique = h.unique
	return req, resp, nil
}

type StatfsRequest struct {
	inHeader
}

const statfsRequestSize = unsafe.Sizeof(StatfsRequest{})

type StatfsResponse struct {
	outHeader
	out kstatfs
}

const statfsResponseSize = unsafe.Sizeof(StatfsResponse{})

func (r *StatfsResponse) Respond(s *RequestScope) {
	d := (*[statfsResponseSize]byte)(unsafe.Pointer(r))
	s.respond(d[:])
}

type UnsupportedRequest struct {
	inHeader
}

type UnsupportedResponse struct {
	outHeader
}

const unsupportedResponseSize = unsafe.Sizeof(UnsupportedResponse{})

func unsupportedResponse(a *Allocator) *UnsupportedResponse {
	return (*UnsupportedResponse)(a.allocPointer(unsupportedResponseSize))
}

func parseUnsupported(b []byte, alloc *Allocator) (*UnsupportedRequest, *UnsupportedResponse, error) {
	req := (*UnsupportedRequest)(unsafe.Pointer(&b[0]))
	resp := unsupportedResponse(alloc)
	resp.unique = req.unique
	return req, resp, nil
}
