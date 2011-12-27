// Copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Adapted from Plan 9 from User Space's src/cmd/9pfuse/fuse.c,
// which carries this notice:
//
// The files in this directory are subject to the following license.
// 
// The author of this software is Russ Cox.
// 
//         Copyright (c) 2006 Russ Cox
// 
// Permission to use, copy, modify, and distribute this software for any
// purpose without fee is hereby granted, provided that this entire notice
// is included in all copies of any software which is or includes a copy
// or modification of this software and in all copies of the supporting
// documentation for such software.
// 
// THIS SOFTWARE IS BEING PROVIDED "AS IS", WITHOUT ANY EXPRESS OR IMPLIED
// WARRANTY.  IN PARTICULAR, THE AUTHOR MAKES NO REPRESENTATION OR WARRANTY
// OF ANY KIND CONCERNING THE MERCHANTABILITY OF THIS SOFTWARE OR ITS
// FITNESS FOR ANY PARTICULAR PURPOSE.

// Package fuse is a message-based library for writing FUSE file systems
// on FreeBSD, Linux, and OS X.
//
// On OS X, it requires OSXFUSE (http://osxfuse.github.com/).
//
package fuse

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

type Conn struct {
	fd int
	buf []byte
}

// Mount mounts a new fuse connection on the named directory
// and returns a connection for reading and writing FUSE messages.
func Mount(dir string) (*Conn, error) {
	fd, errstr := mount(dir)
	if errstr != "" {
		return nil, errors.New(errstr)
	}
	
	return &Conn{fd: fd}, nil
}

// A Request represents a single FUSE request received from the kernel.
// Use a type switch to determine the specific kind.
// A request of unrecognized type will have concrete type *Header.
type Request interface {
	// Hdr returns the Header associated with this request.
	Hdr() *Header
	
	// RespondError responds to the request with the given error number.
	RespondError(err syscall.Errno)

	String() string
}

// A RequestID identifies an active FUSE request.
type RequestID uint64

// A NodeID is a number identifying a directory or file.
// It must be unique among IDs returned in LookupResponses
// that have not yet been forgotten by ForgetRequests.
type NodeID uint64

// A Handle is a number identifying an open directory or file.
// It only needs to be unique while the directory or file is open.
type Handle uint64

// The RootID identifies the root directory of a FUSE file system.
const RootID NodeID = rootID

// A Header describes the basic information sent in every request.
type Header struct {
	Conn *Conn  // connection this request was received on
	ID RequestID  // unique ID for request
	Node NodeID // directoy or file the request is about
	Uid uint32  // user ID of process making request
	Gid uint32  // group ID of process making request
	Pid uint32  // process ID of process making request
}

func (h *Header) String() string {
	return fmt.Sprintf("ID=%#x Node=%#x Uid=%d Gid=%d Pid=%d", h.ID, h.Node, h.Uid, h.Gid, h.Pid)
}

func (h *Header) Hdr() *Header {
	return h
}

func (h *Header) RespondError(err syscall.Errno) {
	// FUSE uses negative errors!
	// TODO: File bug report against OSXFUSE: positive error causes kernel panic.
	out := &outHeader{Error: -int32(err), Unique: uint64(h.ID)}
	h.Conn.respond(out, unsafe.Sizeof(*out))
}

var maxWrite = syscall.Getpagesize()
var bufSize = 4096 + maxWrite

// a message represents the bytes of a single FUSE message
type message struct {
	conn *Conn
	buf []byte  // all bytes
	hdr *inHeader  // header
	off int  // offset for reading additional fields
}

func newMessage(c *Conn) *message {
	m := &message{conn: c, buf: make([]byte, bufSize)}
	m.hdr = (*inHeader)(unsafe.Pointer(&m.buf[0]))
	return m
}

func (m *message) len() uintptr {
	return uintptr(len(m.buf) - m.off)
}

func (m *message) data() unsafe.Pointer {
	var p unsafe.Pointer
	if m.off < len(m.buf) {
		p = unsafe.Pointer(&m.buf[m.off])
	}
	return p
}

func (m *message) bytes() []byte {
	return m.buf[m.off:]
}

func (m *message) Header() Header {
	h := m.hdr
	return Header{Conn: m.conn, ID: RequestID(h.Unique), Node: NodeID(h.Nodeid), Uid: h.Uid, Gid: h.Gid, Pid: h.Pid}
}

func (c *Conn) ReadRequest() (Request, error) {
	// TODO: Some kind of buffer reuse.
	m := newMessage(c)
	n, err := syscall.Read(c.fd, m.buf)
	if err != nil && err != syscall.ENODEV {
		return nil, err
	}
	if n <= 0 {
		return nil, io.EOF
	}
	m.buf = m.buf[:n]
	
	if n < inHeaderSize {
		return nil, errors.New("fuse: message too short")
	}
	
	// FreeBSD FUSE sends a short length in the header
	// for FUSE_INIT even though the actual read length is correct.
	if n == inHeaderSize+initInSize && m.hdr.Opcode == opInit && m.hdr.Len < uint32(n) {
		m.hdr.Len = uint32(n)
	}

	if m.hdr.Len != uint32(n) {
		return nil, fmt.Errorf("fuse: read %d but expected %d", n, m.hdr.Len)
	}

	m.off = inHeaderSize

	// Convert to data structures.
	// Do not trust kernel to hand us well-formed data.
	var req Request
	switch m.hdr.Opcode {
	default:
		println("No opcode", m.hdr.Opcode)
		goto unrecognized

	case opLookup:
		buf := m.bytes()
		n := len(buf)
		if n == 0 || buf[n-1] != '\x00' {
			goto corrupt
		}
		req = &LookupRequest{
			Header: m.Header(),
			Name: string(buf[:n-1]),
		}

	case opForget:
		in := (*forgetIn)(m.data())
		if m.len() < unsafe.Sizeof(*in) {
			goto corrupt
		}
		req = &ForgetRequest{
			Header: m.Header(),
			N: in.Nlookup,
		}

	case opGetattr:
		req = &GetattrRequest{
			Header: m.Header(),
		}

	case opSetattr: panic("opSetattr")
	case opReadlink: panic("opReadlink")
	case opSymlink: panic("opSymlink")
	case opMknod: panic("opMknod")
	case opMkdir: panic("opMkdir")
	case opUnlink: panic("opUnlink")
	case opRmdir: panic("opRmdir")
	case opRename: panic("opRename")
	case opLink: panic("opLink")

	case opOpendir, opOpen:
		in := (*openIn)(m.data())
		if m.len() < unsafe.Sizeof(*in) {
			goto corrupt
		}
		req = &OpenRequest{
			Header: m.Header(),
			Dir: m.hdr.Opcode == opOpendir,
			Flags: in.Flags,
			Mode: in.Mode,
		}

	case opRead, opReaddir:
		in := (*readIn)(m.data())
		fmt.Printf("READ %x\n", m.bytes())
		if m.len() < unsafe.Sizeof(*in) {
			goto corrupt
		}
		req = &ReadRequest{
			Header: m.Header(),
			Dir: m.hdr.Opcode == opReaddir,
			Handle: Handle(in.Fh),
			Offset: int64(in.Offset),
			Size: int(in.Size),
		}

	case opWrite: panic("opWrite")

	case opStatfs:
		req = &StatfsRequest{
			Header: m.Header(),
		}

	case opRelease, opReleasedir:
		in := (*releaseIn)(m.data())
		if m.len() < unsafe.Sizeof(*in) {
			goto corrupt
		}
		req = &ReleaseRequest{
			Header: m.Header(),
			Dir: m.hdr.Opcode == opReleasedir,
			Handle: Handle(in.Fh),
			Flags: in.Flags,
			ReleaseFlags: ReleaseFlags(in.ReleaseFlags),
			LockOwner: in.LockOwner,
		}

	case opFsync: panic("opFsync")

	case opSetxattr:
		var size uint32
		var r *SetxattrRequest
		if runtime.GOOS == "darwin" {
			in := (*setxattrInOSX)(m.data())
			if m.len() < unsafe.Sizeof(*in) {
				goto corrupt
			}
			r = &SetxattrRequest{
				Flags: in.Flags,
				Position: in.Position,
			}
			size = in.Size
			m.off += int(unsafe.Sizeof(*in))
		} else {
			in := (*setxattrIn)(m.data())
			if m.len() < unsafe.Sizeof(*in) {
				goto corrupt
			}
			r = &SetxattrRequest{
			}
			size = in.Size
			m.off += int(unsafe.Sizeof(*in))
		}
		r.Header = m.Header()
		name := m.bytes()
		i := bytes.IndexByte(name, '\x00')
		if i < 0 {
			goto corrupt
		}
		r.Name = string(name[:i])
		r.Xattr = name[i+1:]
		if uint32(len(r.Xattr)) < size {
			goto corrupt
		}
		r.Xattr = r.Xattr[:size]
		req = r
		
	case opGetxattr:
		if runtime.GOOS == "darwin" {
			in := (*getxattrInOSX)(m.data())
			if m.len() < unsafe.Sizeof(*in) {
				goto corrupt
			}
			req = &GetxattrRequest{
				Header: m.Header(),
				Size: in.Size,
				Position: in.Position,
			}
		} else {
			in := (*getxattrIn)(m.data())
			if m.len() < unsafe.Sizeof(*in) {
				goto corrupt
			}
			req = &GetxattrRequest{
				Header: m.Header(),
				Size: in.Size,
			}
		}

	case opListxattr:
		if runtime.GOOS == "darwin" {
			in := (*getxattrInOSX)(m.data())
			if m.len() < unsafe.Sizeof(*in) {
				goto corrupt
			}
			req = &ListxattrRequest{
				Header: m.Header(),
				Size: in.Size,
				Position: in.Position,
			}
		} else {
			in := (*getxattrIn)(m.data())
			if m.len() < unsafe.Sizeof(*in) {
				goto corrupt
			}
			req = &ListxattrRequest{
				Header: m.Header(),
				Size: in.Size,
			}
		}

	case opRemovexattr:
		buf := m.bytes()
		n := len(buf)
		if n == 0 || buf[n-1] != '\x00' {
			goto corrupt
		}
		req = &RemovexattrRequest{
			Header: m.Header(),
			Name: string(buf[:n-1]),
		}

	case opFlush:
		in := (*flushIn)(m.data())
		if m.len() < unsafe.Sizeof(*in) {
			goto corrupt
		}
		req = &FlushRequest{
			Header: m.Header(),
			Handle: Handle(in.Fh),
			Flags: in.FlushFlags,
			LockOwner: in.LockOwner,
		}

	case opInit:
		in := (*initIn)(m.data())
		if m.len() < unsafe.Sizeof(*in) {
			goto corrupt
		}
		req = &InitRequest{
			Header: m.Header(),
			Major: in.Major,
			Minor: in.Minor,
			MaxReadahead: in.MaxReadahead,
			Flags: InitFlags(in.Flags),
		}

	case opFsyncdir: panic("opFsyncdir")
	case opGetlk: panic("opGetlk")
	case opSetlk: panic("opSetlk")
	case opSetlkw: panic("opSetlkw")

	case opAccess:
		in := (*accessIn)(m.data())
		if m.len() < unsafe.Sizeof(*in) {
			goto corrupt
		}
		req = &AccessRequest{
			Header: m.Header(),
			Mask: in.Mask,
		}

	case opCreate: panic("opCreate")
	case opInterrupt: panic("opInterrupt")
	case opBmap: panic("opBmap")

	case opDestroy:
		req = &DestroyRequest{
			Header: m.Header(),
		}

	// OS X
	case opSetvolname: panic("opSetvolname")
	case opGetxtimes: panic("opGetxtimes")
	case opExchange: panic("opExchange")

	/*
	case opUnlink:
		if n == 0 || buf[n-1] != '\x00' {
			goto corrupt
		}
		return &Unlink{Header: *hdr, Name: string(buf[:n-1])}, nil

	case opRmdir:
		if n == 0 || buf[n-1] != '\x00' {
			goto corrupt
		}
		return &Rmdir{Header: *hdr, Name: string(buf[:n-1])}, nil
	...
	*/
	}

	return req, nil

corrupt:
	return nil, fmt.Errorf("fuse: malformed message")

unrecognized:
	// Unrecognized message.
	// Assume higher-level code will send a "no idea what you mean" error.
	h := m.Header()
	return &h, nil
}

func (c *Conn) respond(out *outHeader, n uintptr) {
	out.Len = uint32(n)
	msg := (*[1<<30]byte)(unsafe.Pointer(out))[:n]
	syscall.Write(c.fd, msg)
}

func (c *Conn) respondData(out *outHeader, n uintptr, data []byte) {
	// TODO: use writev
	out.Len = uint32(n + uintptr(len(data)))
	msg := make([]byte, out.Len)
	copy(msg, (*[1<<30]byte)(unsafe.Pointer(out))[:n])
	copy(msg[n:], data)
	syscall.Write(c.fd, msg)
}

// An InitRequest is the first request sent on a FUSE file system.
type InitRequest struct {
	Header
	Major uint32
	Minor uint32
	MaxReadahead uint32
	Flags InitFlags
}

func (r *InitRequest) String() string {
	return fmt.Sprintf("[%s] Init %d.%d ra=%d fl=%s", &r.Header, r.Major, r.Minor, r.MaxReadahead, r.Flags)
}

// An InitResponse is the response to an InitRequest.
type InitResponse struct {
	Major uint32
	Minor uint32
	MaxReadahead uint32
	Flags InitFlags
	MaxWrite uint32
}

func (r *InitResponse) String() string {
	return fmt.Sprintf("Init %+v", *r)
}

// Respond replies to the request with the given response.
func (r *InitRequest) Respond(resp *InitResponse) {
	out := &initOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		Major: resp.Major,
		Minor: resp.Minor,
		MaxReadahead: resp.MaxReadahead,
		Flags: uint32(resp.Flags),
		MaxWrite: resp.MaxWrite,
	}
	r.Conn.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A StatfsRequest requests information about the mounted file system.
type StatfsRequest struct {
	Header
}

func (r *StatfsRequest) String() string {
	return fmt.Sprintf("[%s] Statfs\n", &r.Header)
}

// Respond replies to the request with the given response.
func (r *StatfsRequest) Respond(resp *StatfsResponse) {
	out := &statfsOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		St: kstatfs{
			Blocks: resp.Blocks,
			Bfree: resp.Bfree,
			Bavail: resp.Bavail,
			Files: resp.Files,
			Bsize: resp.Bsize,
			Namelen: resp.Namelen,
			Frsize: resp.Frsize,
		},
	}
	r.Conn.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A StatfsResponse is the response to a StatfsRequest.
type StatfsResponse struct {
	Blocks uint64  // Total data blocks in file system.
	Bfree uint64  // Free blocks in file system.
	Bavail uint64  // Free blocks in file system if you're not root.
	Files uint64  // Total files in file system.
	Ffree uint64  // Free files in file system.
	Bsize uint32  // Block size
	Namelen uint32  // Maximum file name length?
	Frsize uint32  // ?
}

func (r *StatfsResponse) String() string {
	return fmt.Sprintf("Statfs %+v", *r)
}

// An AccessRequest asks whether the file can be accessed
// for the purpose specified by the mask.
type AccessRequest struct {
	Header
	Mask uint32
}

func (r *AccessRequest) String() string {
	return fmt.Sprintf("[%s] Access mask=%#x", &r.Header, r.Mask)
}

// Respond replies to the request indicating that access is allowed.
// To deny access, use RespondError.
func (r *AccessRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.Conn.respond(out, unsafe.Sizeof(*out))
}

// An Attr is the metadata for a single file or directory.
type Attr struct {
	Inode uint64 // inode number
	Size uint64  // size in bytes
	Blocks uint64  // size in blocks
	Atime time.Time  // time of last access
	Mtime time.Time  // time of last modification
	Ctime time.Time // time of last inode change
	Crtime time.Time  // time of creation (OS X only)
	Mode os.FileMode  // file mode
	Nlink uint32  // number of links
	Uid uint32 // owner uid
	Gid uint32 // group gid
	Rdev uint32  // device numbers
	Flags uint32 // chflags(2) flags (OS X only)
}

func unix(t time.Time) (sec uint64, nsec uint32) {
	nano := t.UnixNano()
	sec = uint64(nano/1e9)
	nsec = uint32(nano%1e9)
	return
}

func (a *Attr) attr() (out attr) {
	out.Ino = a.Inode
	out.Size = a.Size
	out.Blocks = a.Blocks
	out.Atime, out.AtimeNsec = unix(a.Atime)
	out.Mtime, out.MtimeNsec = unix(a.Mtime)
	out.Ctime, out.CtimeNsec = unix(a.Ctime)
	out.Crtime, out.CrtimeNsec = unix(a.Crtime)
	out.Mode = uint32(a.Mode)&0777
	switch {
	default:
		out.Mode |= syscall.S_IFREG
	case a.Mode&os.ModeDir != 0:
		out.Mode |= syscall.S_IFDIR
	case a.Mode&os.ModeDevice != 0:
		if a.Mode&os.ModeCharDevice != 0 {
			out.Mode |= syscall.S_IFCHR
		} else {
			out.Mode |= syscall.S_IFBLK
		}
	case a.Mode&os.ModeNamedPipe != 0:
		out.Mode |= syscall.S_IFIFO
	case a.Mode&os.ModeSymlink != 0:
		out.Mode |= syscall.S_IFLNK
	case a.Mode&os.ModeSocket != 0:
		out.Mode |= syscall.S_IFSOCK
	}
	if a.Mode&os.ModeSetuid != 0 {
		out.Mode |= syscall.S_ISUID
	}
	if a.Mode&os.ModeSetgid != 0 {
		out.Mode |= syscall.S_ISGID
	}
	out.Nlink = a.Nlink
	if out.Nlink < 1 {
		out.Nlink = 1
	}
	out.Uid = a.Uid
	out.Gid = a.Gid
	out.Rdev = a.Rdev
	out.Flags = a.Flags

	return
}

// A GetattrRequest asks for the metadata for the file denoted by r.Node.
type GetattrRequest struct {
	Header
}

func (r *GetattrRequest) String() string {
	return fmt.Sprintf("[%s] Getattr", &r.Header)
}

// Respond replies to the request with the given response.
func (r *GetattrRequest) Respond(resp *GetattrResponse) {
	out := &attrOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		AttrValid: uint64(resp.AttrValid/time.Second),
		AttrValidNsec: uint32(resp.AttrValid%time.Second / time.Nanosecond),
		Attr: resp.Attr.attr(),
	}
	r.Conn.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A GetattrResponse is the response to a GetattrRequest.
type GetattrResponse struct {
	AttrValid time.Duration  // how long Attr can be cached
	Attr Attr  // file attributes
}

func (r *GetattrResponse) String() string {
	return fmt.Sprintf("Getattr %+v", *r)
}

// A GetxattrRequest asks for the extended attributes associated with r.Node.
type GetxattrRequest struct {
	Header
	Size uint32  // maximum size to return
	Position uint32  // offset within extended attributes
}

func (r *GetxattrRequest) String() string {
	return fmt.Sprintf("[%s] Getxattr %d @%d", &r.Header, r.Size, r.Position)
}

// Respond replies to the request with the given response.
func (r *GetxattrRequest) Respond(resp *GetxattrResponse) {
	out := &getxattrOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		Size: uint32(len(resp.Xattr)),
	}
	r.Conn.respondData(&out.outHeader, unsafe.Sizeof(*out), resp.Xattr)
}

// A GetxattrResponse is the response to a GetxattrRequest.
type GetxattrResponse struct {
	Xattr []byte
}

func (r *GetxattrResponse) String() string {
	return fmt.Sprintf("Getxattr %x", r.Xattr)
}

// A ListxattrRequest asks to list the extended attributes associated with r.Node.
type ListxattrRequest struct {
	Header
	Size uint32  // maximum size to return
	Position uint32  // offset within attribute list
}

func (r *ListxattrRequest) String() string {
	return fmt.Sprintf("[%s] Listxattr %d @%d", &r.Header, r.Size, r.Position)
}

// Respond replies to the request with the given response.
func (r *ListxattrRequest) Respond(resp *ListxattrResponse) {
	out := &getxattrOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		Size: uint32(len(resp.Xattr)),
	}
	r.Conn.respondData(&out.outHeader, unsafe.Sizeof(*out), resp.Xattr)
}

// A ListxattrResponse is the response to a ListxattrRequest.
type ListxattrResponse struct {
	Xattr []byte
}

func (r *ListxattrResponse) String() string {
	return fmt.Sprintf("Listxattr %x", r.Xattr)
}

// A RemovexattrRequest asks to remove an extended attribute associated with r.Node.
type RemovexattrRequest struct {
	Header
	Name string  // name of extended attribute
}

func (r *RemovexattrRequest) String() string {
	return fmt.Sprintf("[%s] Removexattr %q", &r.Header, r.Name)
}

// Respond replies to the request, indicating that the attribute was removed.
func (r *RemovexattrRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.Conn.respond(out, unsafe.Sizeof(*out))
}

// A SetxattrRequest asks to set an extended attribute associated with a file.
type SetxattrRequest struct {
	Header
	Flags uint32
	Position uint32  // OS X only
	Name string
	Xattr []byte
}

func (r *SetxattrRequest) String() string {
	return fmt.Sprintf("[%s] Setxattr %q %x fl=%#x @%#x", &r.Header, r.Name, r.Xattr, r.Flags, r.Position)
}

// Respond replies to the request, indicating that the extended attribute was set.
func (r *SetxattrRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.Conn.respond(out, unsafe.Sizeof(*out))
}

// A LookupRequest asks to look up the given name in the directory named by r.Node.
type LookupRequest struct {
	Header
	Name string
}

func (r *LookupRequest) String() string {
	return fmt.Sprintf("[%s] Lookup %q", &r.Header, r.Name)
}

// Respond replies to the request with the given response.
func (r *LookupRequest) Respond(resp *LookupResponse) {
	out := &entryOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		Nodeid: uint64(resp.Node),
		Generation: resp.Generation,
		EntryValid: uint64(resp.EntryValid/time.Second),
		EntryValidNsec: uint32(resp.EntryValid%time.Second / time.Nanosecond),
		AttrValid: uint64(resp.AttrValid/time.Second),
		AttrValidNsec: uint32(resp.AttrValid%time.Second / time.Nanosecond),
		Attr: resp.Attr.attr(),
	}
	r.Conn.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A LookupResponse is the response to a LookupRequest.
type LookupResponse struct {
	Node NodeID
	Generation uint64
	EntryValid time.Duration
	AttrValid time.Duration
	Attr Attr
}
 
func (r *LookupResponse) String() string {
	return fmt.Sprintf("Lookup %+v", *r)
}

// An OpenRequest asks to open a file or directory
type OpenRequest struct {
	Header
	Dir bool  // is this Opendir?
	Flags uint32
	Mode uint32
}

func (r *OpenRequest) String() string {
	return fmt.Sprintf("[%s] Open dir=%v fl=%#x mode=%#x", &r.Header, r.Dir, r.Flags, r.Mode)
}

// Respond replies to the request with the given response.
func (r *OpenRequest) Respond(resp *OpenResponse) {
	out := &openOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		Fh: uint64(resp.Handle),
		OpenFlags: uint32(resp.Flags),
	}
	r.Conn.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A OpenResponse is the response to a OpenRequest.
type OpenResponse struct {
	Handle Handle
	Flags OpenFlags
}

func (r *OpenResponse) String() string {
	return fmt.Sprintf("Open %+v", *r)
}

// A ReadRequest asks to read from an open file.
type ReadRequest struct {
	Header
	Dir bool  // is this Readdir?
	Handle Handle
	Offset int64
	Size int
}

func (r *ReadRequest) String() string {
	return fmt.Sprintf("[%s] Read %#x %d @%#x dir=%v", &r.Header, r.Handle, r.Size, r.Offset, r.Dir)
}

// Respond replies to the request with the given response.
func (r *ReadRequest) Respond(resp *ReadResponse) {
	out := &outHeader{Unique: uint64(r.ID)}
	r.Conn.respondData(out, unsafe.Sizeof(*out), resp.Data)
}

// A ReadResponse is the response to a ReadRequest.
type ReadResponse struct {
	Data []byte
}

func (r *ReadResponse) String() string {
	return fmt.Sprintf("Read %x", r.Data)
}

// A ReleaseRequest asks to release (close) an open file handle.
type ReleaseRequest struct {
	Header
	Dir bool  // is this Releasedir?
	Handle Handle
	Flags uint32  // flags from OpenRequest
	ReleaseFlags ReleaseFlags
	LockOwner uint32
}

func (r *ReleaseRequest) String() string {
	return fmt.Sprintf("[%s] Release %#x fl=%#x rfl=%v owner=%#x", &r.Header, r.Handle, r.Flags, r.ReleaseFlags, r.LockOwner)
}

// Respond replies to the request, indicating that the handle has been released.
func (r *ReleaseRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.Conn.respond(out, unsafe.Sizeof(*out))
}

// A DestroyRequest is sent by the kernel when unmounting the file system.
// No more requests will be received after this one, but it should still be
// responded to.
type DestroyRequest struct {
	Header
}

func (r *DestroyRequest) String() string {
	return fmt.Sprintf("[%s] Destroy", &r.Header)
}

// Respond replies to the request.
func (r *DestroyRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.Conn.respond(out, unsafe.Sizeof(*out))
}

// A ForgetRequest is sent by the kernel when forgetting about r.Node
// as returned by r.N lookup requests.
type ForgetRequest struct {
	Header
	N uint64
}

func (r *ForgetRequest) String() string {
	return fmt.Sprintf("[%s] Forget %d", &r.Header, r.N)
}

// Respond replies to the request, indicating that the forgetfulness has been recorded.
func (r *ForgetRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.Conn.respond(out, unsafe.Sizeof(*out))
}

// A Dirent represents a single directory entry.
type Dirent struct {
	Offset uint64  // offset of this entry in directory

	Inode uint64  // inode this entry names
	Type uint32  // ?
	Name string  // name of entry
}

// AppendDirent appends the encoded form of a directory entry to data
// and returns the resulting slice.
func AppendDirent(data []byte, dir Dirent) []byte {
	de := dirent{
		Ino: dir.Inode,
		Namelen: uint32(len(dir.Name)),
		Type: dir.Type,
	}
	de.Off = uint64( len(data) + direntSize + (len(dir.Name)+7)&^7)
	data = append(data, (*[direntSize]byte)(unsafe.Pointer(&de))[:]...)
	data = append(data, dir.Name...)
	n := direntSize + uintptr(len(dir.Name))
	if n%8 != 0 {
		var pad [8]byte
		data = append(data, pad[:8-n%8]...)
	}
	return data	
}

// A FlushRequest asks to flush XXX.
type FlushRequest struct {
	Header
	Handle Handle
	Flags uint32
	LockOwner uint64
}

func (r *FlushRequest) String() string {
	return fmt.Sprintf("[%s] Flush %#x fl=%#x lk=%#x", &r.Header, r.Handle, r.Flags, r.LockOwner)
}

// Respond replies to the request indicating that the node has been flushed.
func (r *FlushRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.Conn.respond(out, unsafe.Sizeof(*out))
}


/*{

// A XXXRequest xxx.
type XXXRequest struct {
	Header
	xxx
}

func (r *XXXRequest) String() string {
	return fmt.Sprintf("[%s] XXX xxx", &r.Header)
}

// Respond replies to the request with the given response.
func (r *XXXRequest) Respond(resp *XXXResponse) {
	out := &xxxOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		xxx,
	}
	r.Conn.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A XXXResponse is the response to a XXXRequest.
type XXXResponse struct {
	xxx
}

func (r *XXXResponse) String() string {
	return fmt.Sprintf("XXX %+v", *r)
}

}
*/
