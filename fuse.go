// See the file LICENSE for copyright and licensing information.

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

// Package fuse enables writing FUSE file systems on Linux, OS X, and FreeBSD.
//
// This is adapted from http://github.com/bazil/fuse aka bazil.org/fuse
//
// Errors
//
// Operations can return errors. The FUSE interface can only
// communicate POSIX errno error numbers to file system clients, the
// message is not visible to file system clients. The returned error
// can implement ErrorNumber to control the errno returned. Without
// ErrorNumber, a generic errno (EIO) is returned.
//
// Error messages will be visible in the debug log as part of the
// response.
//
// Interrupted Operations
//
// In some file systems, some operations
// may take an undetermined amount of time.  For example, a Read waiting for
// a network message or a matching Write might wait indefinitely.  If the request
// is cancelled and no longer needed, the context will be cancelled.
// Blocking operations should select on a receive from ctx.Done() and attempt to
// abort the operation early if the receive succeeds (meaning the channel is closed).
// To indicate that the operation failed because it was aborted, return fuse.EINTR.
//
// If an operation does not block for an indefinite amount of time, supporting
// cancellation is not necessary.
//
// Authentication
//
// All requests types embed a Header, meaning that the method can
// inspect req.Pid, req.Uid, and req.Gid as necessary to implement
// permission checking. The kernel FUSE layer normally prevents other
// users from accessing the FUSE file system (to change this, see
// AllowOther, AllowRoot), but does not enforce access modes (to
// change this, see DefaultPermissions).
//
// Mount Options
//
// Behavior and metadata of the mounted file system can be changed by
// passing MountOption values to Mount.
//
package fuse // import "github.com/scootdev/fuse"

import (
	"fmt"
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// A RequestID identifies an active FUSE request.
type RequestID uint64

func (r RequestID) String() string {
	return fmt.Sprintf("%#x", uint64(r))
}

// A NodeID is a number identifying a directory or file.
// It must be unique among IDs returned in LookupResponses
// that have not yet been forgotten by ForgetRequests.
type NodeID uint64

func (n NodeID) String() string {
	return fmt.Sprintf("%#x", uint64(n))
}

// A HandleID is a number identifying an open directory or file.
// It only needs to be unique while the directory or file is open.
type HandleID uint64

func (h HandleID) String() string {
	return fmt.Sprintf("%#x", uint64(h))
}

// The RootID identifies the root directory of a FUSE file system.
const RootID NodeID = rootID

// An ErrorNumber is an error with a specific error number.
//
// Operations may return an error value that implements ErrorNumber to
// control what specific error number (errno) to return.
type ErrorNumber interface {
	// Errno returns the the error number (errno) for this error.
	Errno() Errno
}

const (
	// ENOSYS indicates that the call is not supported.
	ENOSYS = Errno(syscall.ENOSYS)

	// ESTALE is used by Serve to respond to violations of the FUSE protocol.
	ESTALE = Errno(syscall.ESTALE)

	ENOENT = Errno(syscall.ENOENT)
	EIO    = Errno(syscall.EIO)
	EPERM  = Errno(syscall.EPERM)

	// EINTR indicates request was interrupted by an InterruptRequest.
	// See also fs.Intr.
	EINTR = Errno(syscall.EINTR)

	ERANGE  = Errno(syscall.ERANGE)
	ENOTSUP = Errno(syscall.ENOTSUP)
	EEXIST  = Errno(syscall.EEXIST)
)

// DefaultErrno is the errno used when error returned does not
// implement ErrorNumber.
const DefaultErrno = EIO

var errnoNames = map[Errno]string{
	ENOSYS: "ENOSYS",
	ESTALE: "ESTALE",
	ENOENT: "ENOENT",
	EIO:    "EIO",
	EPERM:  "EPERM",
	EINTR:  "EINTR",
	EEXIST: "EEXIST",
}

// Errno implements Error and ErrorNumber using a syscall.Errno.
type Errno syscall.Errno

var _ = ErrorNumber(Errno(0))
var _ = error(Errno(0))

func (e Errno) Errno() Errno {
	return e
}

func (e Errno) String() string {
	return syscall.Errno(e).Error()
}

func (e Errno) Error() string {
	return syscall.Errno(e).Error()
}

// ErrnoName returns the short non-numeric identifier for this errno.
// For example, "EIO".
func (e Errno) ErrnoName() string {
	s := errnoNames[e]
	if s == "" {
		s = fmt.Sprint(e.Errno())
	}
	return s
}

// fileMode returns a Go os.FileMode from a Unix mode.
func fileMode(unixMode uint32) os.FileMode {
	mode := os.FileMode(unixMode & 0777)
	switch unixMode & syscall.S_IFMT {
	case syscall.S_IFREG:
		// nothing
	case syscall.S_IFDIR:
		mode |= os.ModeDir
	case syscall.S_IFCHR:
		mode |= os.ModeCharDevice | os.ModeDevice
	case syscall.S_IFBLK:
		mode |= os.ModeDevice
	case syscall.S_IFIFO:
		mode |= os.ModeNamedPipe
	case syscall.S_IFLNK:
		mode |= os.ModeSymlink
	case syscall.S_IFSOCK:
		mode |= os.ModeSocket
	default:
		// no idea
		mode |= os.ModeDevice
	}
	if unixMode&syscall.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if unixMode&syscall.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	return mode
}

type noOpcode struct {
	Opcode uint32
}

func (m noOpcode) String() string {
	return fmt.Sprintf("No opcode %v", m.Opcode)
}

func parseBuf(b []byte, alloc *Allocator, c *Conn) (Request, Response, error) {
	h := (*inHeader)(unsafe.Pointer(&b[0]))
	if h.len != uint32(len(b)) {
		switch {
		// FreeBSD FUSE sends a short length in the header
		// for FUSE_INIT even though the actual read length is correct.
		case h.opcode == OpInit && len(b) == int(inHeaderSize+initInSize) && h.len < uint32(len(b)):
			h.len = uint32(len(b))
		default:
			return nil, nil, fmt.Errorf("fuse: read %d opcode %d but expected %d", len(b), h.opcode, h.len)
		}
	}
	switch h.opcode {
	case OpInit:
		return parseInit(b, alloc)
	case OpStatfs:
		return parseStatfs(b, alloc)
	case OpGetattr:
		return parseGetattr(b, alloc, c.proto)
	case OpGetxattr, OpListxattr, OpFlush:
		return parseUnsupported(b, alloc)
	case OpLookup:
		return parseLookup(b, alloc)
	case OpOpendir:
		return parseOpendir(b, alloc)
	case OpReaddir:
		return parseReaddir(b, alloc, c.proto)
	case OpReleasedir:
		return parseReleasedir(b, alloc)
	case OpReadlink:
		return parseReadlink(b, alloc)
	case OpOpen:
		return parseOpen(b, alloc)
	case OpRead:
		return parseRead(b, alloc, c.proto)
	case OpRelease:
		return parseRelease(b, alloc)
	default:
		log.Printf("Unknown request type %v", h.opcode)
		return nil, nil, fmt.Errorf("Not implemented")
	}
}

// TODO(dbentley): Attr is written outside fuse and then we have to copy it in.
// We should change it to the pattern, like all the Request/Response objects, where it has
// no public field, but just methods to set each field.
// This is slightly complicated, because the translation is non obvious. (It currently lives
// in the function Attr.attr, which copies an Attr into an attr.
// An Attr is the metadata for a single file or directory.
type Attr struct {
	Valid time.Duration // how long Attr can be cached

	Inode     uint64      // inode number
	Size      uint64      // size in bytes
	Blocks    uint64      // size in 512-byte units
	Atime     time.Time   // time of last access
	Mtime     time.Time   // time of last modification
	Ctime     time.Time   // time of last inode change
	Crtime    time.Time   // time of creation (OS X only)
	Mode      os.FileMode // file mode
	Nlink     uint32      // number of links
	Uid       uint32      // owner uid
	Gid       uint32      // group gid
	Rdev      uint32      // device numbers
	Flags     uint32      // chflags(2) flags (OS X only)
	BlockSize uint32      // preferred blocksize for filesystem I/O
}

func (a Attr) String() string {
	return fmt.Sprintf("valid=%v ino=%v size=%d mode=%v", a.Valid, a.Inode, a.Size, a.Mode)
}

func unix(t time.Time) (sec uint64, nsec uint32) {
	nano := t.UnixNano()
	sec = uint64(nano / 1e9)
	nsec = uint32(nano % 1e9)
	return
}

func (a *Attr) attr(out *attr) {
	out.Ino = a.Inode
	out.Size = a.Size
	out.Blocks = a.Blocks
	out.Atime, out.AtimeNsec = unix(a.Atime)
	out.Mtime, out.MtimeNsec = unix(a.Mtime)
	out.Ctime, out.CtimeNsec = unix(a.Ctime)
	out.SetCrtime(unix(a.Crtime))
	out.Mode = uint32(a.Mode) & 0777
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
	out.Uid = a.Uid
	out.Gid = a.Gid
	out.Rdev = a.Rdev
	out.SetFlags(a.Flags)
	out.Blksize = a.BlockSize

	return
}

// TODO(dbentley): change Dirent into a type so we don't have to copy so much around
// A Dirent represents a single directory entry.
type Dirent struct {
	// Inode this entry names.
	Inode uint64

	// Type of the entry, for example DT_File.
	//
	// Setting this is optional. The zero value (DT_Unknown) means
	// callers will just need to do a Getattr when the type is
	// needed. Providing a type can speed up operations
	// significantly.
	Type DirentType

	// Name of the entry
	Name string
}

// An overestimate of how much space this will take
func (d *Dirent) Size() int {
	return direntSize + len(d.Name) + 8
}

// Type of an entry in a directory listing.
type DirentType uint32

const (
	// These don't quite match os.FileMode; especially there's an
	// explicit unknown, instead of zero value meaning file. They
	// are also not quite syscall.DT_*; nothing says the FUSE
	// protocol follows those, and even if they were, we don't
	// want each fs to fiddle with syscall.

	// The shift by 12 is hardcoded in the FUSE userspace
	// low-level C library, so it's safe here.

	DT_Unknown DirentType = 0
	DT_Socket  DirentType = syscall.S_IFSOCK >> 12
	DT_Link    DirentType = syscall.S_IFLNK >> 12
	DT_File    DirentType = syscall.S_IFREG >> 12
	DT_Block   DirentType = syscall.S_IFBLK >> 12
	DT_Dir     DirentType = syscall.S_IFDIR >> 12
	DT_Char    DirentType = syscall.S_IFCHR >> 12
	DT_FIFO    DirentType = syscall.S_IFIFO >> 12
)

func (t DirentType) String() string {
	switch t {
	case DT_Unknown:
		return "unknown"
	case DT_Socket:
		return "socket"
	case DT_Link:
		return "link"
	case DT_File:
		return "file"
	case DT_Block:
		return "block"
	case DT_Dir:
		return "dir"
	case DT_Char:
		return "char"
	case DT_FIFO:
		return "fifo"
	}
	return "invalid"
}

// AppendDirent appends the encoded form of a directory entry to data
// and returns the resulting slice.
func AppendDirent(data []byte, dir Dirent) []byte {
	de := dirent{
		Ino:     dir.Inode,
		Namelen: uint32(len(dir.Name)),
		Type:    uint32(dir.Type),
	}
	de.Off = uint64(len(data) + direntSize + (len(dir.Name)+7)&^7)
	data = append(data, (*[direntSize]byte)(unsafe.Pointer(&de))[:]...)
	data = append(data, dir.Name...)
	n := direntSize + uintptr(len(dir.Name))
	if n%8 != 0 {
		var pad [8]byte
		data = append(data, pad[:8-n%8]...)
	}
	return data
}
