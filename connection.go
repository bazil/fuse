package fuse

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"log"
	"syscall"
	"unsafe"
)

// A Conn represents a connection to a mounted FUSE file system.
type Conn struct {
	// Ready is closed when the mount is complete or has failed.
	Ready <-chan struct{}

	// MountError stores any error from the mount process. Only valid
	// after Ready is closed.
	MountError error

	// File handle for kernel communication. Only safe to access if
	// rio or wio is held.
	dev *os.File
	wio sync.RWMutex
	rio sync.RWMutex

	// Protocol version negotiated with InitRequest/InitResponse.
	proto Protocol
}

// MountpointDoesNotExistError is an error returned when the
// mountpoint does not exist.
type MountpointDoesNotExistError struct {
	Path string
}

func (e *MountpointDoesNotExistError) Error() string {
	return fmt.Sprintf("mountpoint does not exist: %v", e.Path)
}

var _ error = (*MountpointDoesNotExistError)(nil)

// Mount mounts a new FUSE connection on the named directory
// and returns a connection for reading and writing FUSE messages.
//
// After a successful return, caller must call Close to free
// resources.
//
// Even on successful return, the new mount is not guaranteed to be
// visible until after Conn.Ready is closed. See Conn.MountError for
// possible errors. Incoming requests on Conn must be served to make
// progress.
func Mount(dir string, mem *Allocator, options ...MountOption) (*Conn, error) {
	conf := mountConfig{
		options: make(map[string]string),
	}
	for _, option := range options {
		if err := option(&conf); err != nil {
			return nil, err
		}
	}

	ready := make(chan struct{}, 1)
	c := &Conn{
		Ready: ready,
	}
	f, err := mount(dir, &conf, ready, &c.MountError)
	if err != nil {
		return nil, err
	}
	c.dev = f

	if err := initMount(c, &conf, mem); err != nil {
		c.Close()
		if err == ErrClosedWithoutInit {
			// see if we can provide a better error
			<-c.Ready
			if err := c.MountError; err != nil {
				return nil, err
			}
		}
		return nil, err
	}

	return c, nil
}

type OldVersionError struct {
	Kernel     Protocol
	LibraryMin Protocol
}

func (e *OldVersionError) Error() string {
	return fmt.Sprintf("kernel FUSE version is too old: %v < %v", e.Kernel, e.LibraryMin)
}

var (
	ErrClosedWithoutInit = errors.New("fuse connection closed without init")
)

func initMount(c *Conn, conf *mountConfig, a *Allocator) error {
	scope, err := c.Read(a)

	if err != nil {
		if err == io.EOF {
			return ErrClosedWithoutInit
		}
		return err
	}
	req, ok := scope.Req.(*initRequest)
	if !ok {
		return fmt.Errorf("missing init; got %T %v", scope.Req, scope.Req)
	}
	resp := scope.Resp.(*initResponse)

	min := Protocol{protoVersionMinMajor, protoVersionMinMinor}
	if req.Kernel().LT(min) {
		resp.RespondError(Errno(syscall.EPROTO), scope)
		c.Close()
		return &OldVersionError{
			Kernel:     req.Kernel(),
			LibraryMin: min,
		}
	}

	proto := Protocol{protoVersionMaxMajor, protoVersionMaxMinor}
	if req.Kernel().LT(proto) {
		// Kernel doesn't support the latest version we have.
		proto = req.Kernel()
	}
	c.proto = proto

	resp.Library(proto)
	resp.MaxReadahead(conf.maxReadahead)
	resp.MaxWrite(128 * 1024)
	resp.Flags(InitBigWrites | conf.initFlags)
	resp.Respond(scope)
	return nil
}

type malformedMessage struct {
}

func (malformedMessage) String() string {
	return "malformed message"
}

// Close closes the FUSE connection.
func (c *Conn) Close() error {
	c.wio.Lock()
	defer c.wio.Unlock()
	c.rio.Lock()
	defer c.rio.Unlock()
	return c.dev.Close()
}

// caller must hold wio or rio
func (c *Conn) fd() int {
	return int(c.dev.Fd())
}

func (c *Conn) Protocol() Protocol {
	return c.proto
}

func (c *Conn) Read(alloc *Allocator) (*RequestScope, error) {
	scope := alloc.newRequest(c)
	buf := alloc.alloc(bufSize)
	n, err := c.read(buf)
	if err != nil {
		return nil, err
	}

	scope.alloc.free(int(bufSize) - n)
	buf = buf[0:n]

	req, resp, err := parseBuf(buf, alloc, c)
	if err != nil {
		return nil, err
	}
	scope.Req = req
	scope.Resp = resp
	return scope, nil
}

func (c *Conn) read(b []byte) (int, error) {
	for {
		c.rio.RLock()
		n, err := syscall.Read(c.fd(), b)
		c.rio.RUnlock()
		if err == syscall.EINTR {
			// OSXFUSE sends EINTR to userspace when a request interrupt
			// completed before it got sent to userspace?
			continue
		}
		if err != nil && err != syscall.ENODEV {
			return n, err
		}
		if n <= 0 {
			return n, io.EOF
		}

		if n < int(inHeaderSize) {
			return 0, errors.New("fuse: message too short")
		}
		return n, nil
	}
}

type bugShortKernelWrite struct {
	Written int64
	Length  int64
	Error   string
	Stack   string
}

func (b bugShortKernelWrite) String() string {
	return fmt.Sprintf("short kernel write: written=%d/%d error=%q stack=\n%s", b.Written, b.Length, b.Error, b.Stack)
}

type bugKernelWriteError struct {
	Error string
	Stack string
}

func (b bugKernelWriteError) String() string {
	return fmt.Sprintf("kernel write error: error=%q stack=\n%s", b.Error, b.Stack)
}

// safe to call even with nil error
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (c *Conn) writeToKernel(msg []byte) error {
	out := (*outHeader)(unsafe.Pointer(&msg[0]))
	out.len = uint32(len(msg))

	c.wio.RLock()
	defer c.wio.RUnlock()
	if Trace {
		log.Print("fuse done")
	}
	nn, err := syscall.Write(c.fd(), msg)
	if err == nil && nn != len(msg) {
		Debug(bugShortKernelWrite{
			Written: int64(nn),
			Length:  int64(len(msg)),
			Error:   errorString(err),
			Stack:   stack(),
		})
	}
	return err
}

func (c *Conn) Respond(msg []byte) {
	if err := c.writeToKernel(msg); err != nil {
		Debug(bugKernelWriteError{
			Error: errorString(err),
			Stack: stack(),
		})
	}
}

type notCachedError struct{}

func (notCachedError) Error() string {
	return "node not cached"
}

var _ ErrorNumber = notCachedError{}

func (notCachedError) Errno() Errno {
	// Behave just like if the original syscall.ENOENT had been passed
	// straight through.
	return ENOENT
}

var (
	ErrNotCached = notCachedError{}
)
