// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// FUSE service loop, for servers that wish to use it.

package fs

import (
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"path"
	"sync"
	"syscall"
	"time"
)

import (
	"bazil.org/fuse"
)

// TODO: FINISH DOCS

// An Intr is a channel that signals that a request has been interrupted.
// Being able to receive from the channel means the request has been
// interrupted.
type Intr chan struct{}

func (Intr) String() string { return "fuse.Intr" }

// An FS is the interface required of a file system.
//
//	Root() (Node, Error)
//
// Root is called to obtain the Node for the file system root.
//
// Optional Methods
//
// An FS implementation may implement
// additional methods to handle the corresponding FUSE requests:
//
//	Init(req *InitRequest, resp *InitResponse) Error
//
// Init is called to initialize the FUSE connection.
// It can inspect the request and adjust the response as desired.
// The default response sets MaxReadahead to 0 and MaxWrite to 4096.
// Init must return promptly.
//
//	Statfs(resp *StatfsResponse, intr Intr) Error
//
// Statfs is called to obtain file system metadata.  It should write that data to resp.
//
//	Rename(req *RenameRequest, intr Intr) Error
//
// XXXX this is not implemented like this. Instead, Rename is a method
// on the source dierctory node, and takes a newDir Node parameter. Fix it like this?
// Rename is called to rename the file req.OldName in the directory req.OldDir to
// become the file req.NewName in the directory req.NewDir.
//
type FS interface {
	Root() (Node, fuse.Error)
}

// A Node is the interface required of a file or directory.
// See the documentation for type FS for general information
// pertaining to all methods.
//
//	Getattr(resp *GetattrResponse, intr Intr) fuse.Error
//
// Getattr obtains the standard metadata for the receiver.
// It should store that metadata in resp.
//
//	Open(xxx, intr Intr) (Handle, fuse.Error)
//
// Open opens the receiver.
// XXX note about access.  XXX OpenFlags.
// XXX note that the Node may be a file or directory.
//
// Optional Methods
//
// An Node implementation may implement additional methods
// to handle the corresponding FUSE requests.
//
// These optional requests can be called for both file and directory nodes:
//
//	Access
//
// Access checks whether the calling context has permission for
// the given operations on the receiver.  If so, Access should return nil.  If not, Access should
// return EPERM.  Note that this call affects the result of the access(2) system call
// but not the open(2) system call.  If Access is not implemented, the Node behaves
// as if it always returns nil (permission granted), relying on checks in Open instead.
//
//	Getxattr
//
// Getxattr obtains an extended attribute for the receiver.
// XXX
//
//	Listxattr
// 
// Listxattr lists the extended attributes recorded for the receiver.
//
//	Removexattr
//
// Removexattr removes an extended attribute from the receiver.
//
//	Setattr
//
// Setattr sets the standard metadata for the receiver.
//
//	Setxattr
//
// Setxattr sets an extended attribute for the receiver.
//
// Optional Directory Methods
//
// These optional requests will be called only for directory nodes:
//
//	Create(xxx)
//
// Create creates 
//
//	Link(xxx)
//
// Link XXX
//
//	Lookup(name string, intr Intr) (Node, Error)
//
// Lookup looks up a specific entry in the receiver,
// which must be a directory.  Lookup should return a Node
// corresponding to the entry.  If the name does not exist in
// the directory, Lookup should return nil, err.
//
// Lookup need not to handle the names "." and "..".
//
//	Mkdir
//
// Mkdir creates XXX
//
//	Mknod XXX
//
// XXX
//
//	Remove
//
// Remove removes the entry with the given name from
// the receiver, which must be a directory.  The entry to be removed
// may correspond to a file (unlink) or to a directory (rmdir).
//
//	Symlink
//
// Symlink creates a new symbolic link in the receiver, which must be a directory.
// The entry 
//
// Optional Symlink Methods
//
// This optional request will be called only for symbolic link nodes:
//
//	Readlink
//
// Readlink reads a symbolic link.
type Node interface {
	Attr() fuse.Attr
}

var startTime = time.Now()

func nodeAttr(n Node) (attr fuse.Attr) {
	attr = n.Attr()
	if attr.Nlink == 0 {
		attr.Nlink = 1
	}
	if attr.Atime.IsZero() {
		attr.Atime = startTime
	}
	if attr.Mtime.IsZero() {
		attr.Mtime = startTime
	}
	if attr.Ctime.IsZero() {
		attr.Ctime = startTime
	}
	if attr.Crtime.IsZero() {
		attr.Crtime = startTime
	}
	return
}

// A Handle is the interface required of an opened file or directory.
// See the documentation for type FS for general information
// pertaining to all methods.
//
//	Flush
//
// Flush is called each time the file or directory is closed.  Because there can be
// multiple file descriptors referring to a single opened file, Flush can be called
// multiple times.
//
// Optional Methods
//
// A Handle implementation may implement additional methods to handle
// the corresponding FUSE requests.  The most common to implement are
// Read, ReadDir, and Write.
//
//	Fsync
//
//	Getlk
//
//	Read
//
//	ReadDir
//
//	Release
//
//	Setlk
//
//	Setlkw
//
//	Write
//
type Handle interface {
}

// Serve serves the FUSE connection by making calls to the methods
// of fs and the Nodes and Handles it makes available.  It returns only
// when the connection has been closed or an unexpected error occurs.
func Serve(c *fuse.Conn, fs FS) error {
	sc := serveConn{}
	sc.req = map[fuse.RequestID]*serveRequest{}

	root, err := fs.Root()
	if err != nil {
		return fmt.Errorf("cannot obtain root node: %v", syscall.Errno(err.(fuse.Errno)).Error())
	}
	sc.node = append(sc.node, nil, &serveNode{name: "/", node: root})
	sc.handle = append(sc.handle, nil)

	for {
		req, err := c.ReadRequest()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		go sc.serve(fs, req)
	}
	return nil
}

type serveConn struct {
	meta        sync.Mutex
	req         map[fuse.RequestID]*serveRequest
	node        []*serveNode
	handle      []*serveHandle
	freeNode    []fuse.NodeID
	freeHandle  []fuse.HandleID
	nodeGen     uint64
	nodeHandles []map[fuse.HandleID]bool // open handles for a node; slice index is NodeID
}

type serveRequest struct {
	Request fuse.Request
	Intr    Intr
}

type serveNode struct {
	name string
	node Node
}

func (sn *serveNode) attr() (attr fuse.Attr) {
	attr = nodeAttr(sn.node)
	if attr.Inode == 0 {
		attr.Inode = hash(sn.name)
	}
	return
}

func hash(s string) uint64 {
	f := fnv.New64()
	f.Write([]byte(s))
	return f.Sum64()
}

type serveHandle struct {
	handle    Handle
	readData  []byte
	trunc     bool
	writeData []byte
	nodeID    fuse.NodeID
}

func (c *serveConn) saveNode(name string, node Node) (id fuse.NodeID, gen uint64, sn *serveNode) {
	sn = &serveNode{name: name, node: node}
	c.meta.Lock()
	if n := len(c.freeNode); n > 0 {
		id = c.freeNode[n-1]
		c.freeNode = c.freeNode[:n-1]
		c.node[id] = sn
		c.nodeGen++
	} else {
		id = fuse.NodeID(len(c.node))
		c.node = append(c.node, sn)
	}
	gen = c.nodeGen
	c.meta.Unlock()
	return
}

func (c *serveConn) saveHandle(handle Handle, nodeID fuse.NodeID) (id fuse.HandleID, shandle *serveHandle) {
	c.meta.Lock()
	shandle = &serveHandle{handle: handle, nodeID: nodeID}
	if n := len(c.freeHandle); n > 0 {
		id = c.freeHandle[n-1]
		c.freeHandle = c.freeHandle[:n-1]
		c.handle[id] = shandle
	} else {
		id = fuse.HandleID(len(c.handle))
		c.handle = append(c.handle, shandle)
	}

	// Update mapping from node ID -> set of open Handle IDs.
	for len(c.nodeHandles) <= int(nodeID) {
		c.nodeHandles = append(c.nodeHandles, nil)
	}
	if c.nodeHandles[nodeID] == nil {
		c.nodeHandles[nodeID] = make(map[fuse.HandleID]bool)
	}
	c.nodeHandles[nodeID][id] = true

	c.meta.Unlock()
	return
}

func (c *serveConn) dropNode(id fuse.NodeID) {
	c.meta.Lock()
	c.node[id] = nil
	if len(c.nodeHandles) > int(id) {
		c.nodeHandles[id] = nil
	}
	c.freeNode = append(c.freeNode, id)
	c.meta.Unlock()
}

func (c *serveConn) dropHandle(id fuse.HandleID) {
	c.meta.Lock()
	h := c.handle[id]
	delete(c.nodeHandles[h.nodeID], id)
	c.handle[id] = nil
	c.freeHandle = append(c.freeHandle, id)
	c.meta.Unlock()
}

// Returns nil for invalid handles.
func (c *serveConn) getHandle(id fuse.HandleID) (shandle *serveHandle) {
	c.meta.Lock()
	defer c.meta.Unlock()
	if id < fuse.HandleID(len(c.handle)) {
		shandle = c.handle[uint(id)]
	}
	if shandle == nil {
		println("missing handle", id, len(c.handle), shandle)
	}
	return
}

func (c *serveConn) serve(fs FS, r fuse.Request) {
	intr := make(Intr)
	req := &serveRequest{Request: r, Intr: intr}

	fuse.Debugf("<- %s", req)
	var node Node
	var snode *serveNode
	c.meta.Lock()
	hdr := r.Hdr()
	if id := hdr.Node; id != 0 {
		if id < fuse.NodeID(len(c.node)) {
			snode = c.node[uint(id)]
		}
		if snode == nil {
			c.meta.Unlock()
			println("missing node", id, len(c.node), snode)
			fuse.Debugf("-> %#x %v", hdr.ID, fuse.ESTALE)
			r.RespondError(fuse.ESTALE)
			return
		}
		node = snode.node
	}
	if c.req[hdr.ID] != nil {
		// This happens with OSXFUSE.  Assume it's okay and
		// that we'll never see an interrupt for this one.
		// Otherwise everything wedges.  TODO: Report to OSXFUSE?
		intr = nil
	} else {
		c.req[hdr.ID] = req
	}
	c.meta.Unlock()

	// Call this before responding.
	// After responding is too late: we might get another request
	// with the same ID and be very confused.
	done := func(resp interface{}) {
		fuse.Debugf("-> %#x %v", hdr.ID, resp)
		c.meta.Lock()
		c.req[hdr.ID] = nil
		c.meta.Unlock()
	}

	switch r := r.(type) {
	default:
		// Note: To FUSE, ENOSYS means "this server never implements this request."
		// It would be inappropriate to return ENOSYS for other operations in this
		// switch that might only be unavailable in some contexts, not all.
		done(fuse.ENOSYS)
		r.RespondError(fuse.ENOSYS)

	// FS operations.
	case *fuse.InitRequest:
		s := &fuse.InitResponse{
			MaxWrite: 4096,
		}
		if fs, ok := fs.(interface {
			Init(*fuse.InitRequest, *fuse.InitResponse, Intr) fuse.Error
		}); ok {
			if err := fs.Init(r, s, intr); err != nil {
				done(err)
				r.RespondError(err)
				break
			}
		}
		done(s)
		r.Respond(s)

	case *fuse.StatfsRequest:
		s := &fuse.StatfsResponse{}
		if fs, ok := fs.(interface {
			Statfs(*fuse.StatfsRequest, *fuse.StatfsResponse, Intr) fuse.Error
		}); ok {
			if err := fs.Statfs(r, s, intr); err != nil {
				done(err)
				r.RespondError(err)
				break
			}
		}
		done(s)
		r.Respond(s)

	// Node operations.
	case *fuse.GetattrRequest:
		s := &fuse.GetattrResponse{}
		if n, ok := node.(interface {
			Getattr(*fuse.GetattrRequest, *fuse.GetattrResponse, Intr) fuse.Error
		}); ok {
			if err := n.Getattr(r, s, intr); err != nil {
				done(err)
				r.RespondError(err)
				break
			}
		} else {
			s.AttrValid = 1 * time.Minute
			s.Attr = snode.attr()
		}
		done(s)
		r.Respond(s)

	case *fuse.SetattrRequest:
		s := &fuse.SetattrResponse{}

		// Special-case truncation, if no other bits are set
		// and the open Handles all have a WriteAll method.
		//
		// TODO WriteAll mishandles all kinds of cases, e.g.
		// truncating to non-zero size, noncontiguous writes, etc
		if r.Valid&fuse.SetattrSize != 0 && r.Size == 0 {
			type writeAll interface {
				WriteAll([]byte, Intr) fuse.Error
			}
			switch r.Valid {
			case fuse.SetattrLockOwner | fuse.SetattrSize, fuse.SetattrSize:
				// Seen on Linux. Handle isn't set.
				c.meta.Lock()
				// it is not safe to assume any handles are open for
				// this node; calls to truncate(2) can happen at any
				// time
				if len(c.nodeHandles) > int(hdr.Node) {
					for hid := range c.nodeHandles[hdr.Node] {
						shandle := c.handle[hid]
						if _, ok := shandle.handle.(writeAll); ok {
							shandle.trunc = true
						}
					}
				}
				c.meta.Unlock()
			case fuse.SetattrHandle | fuse.SetattrSize:
				// Seen on OS X; the Handle is provided.
				shandle := c.getHandle(r.Handle)
				if shandle == nil {
					fuse.Debugf("-> %#x %v", hdr.ID, fuse.ESTALE)
					r.RespondError(fuse.ESTALE)
					return
				}
				if _, ok := shandle.handle.(writeAll); ok {
					shandle.trunc = true
				}
			}
		}

		log.Printf("setattr %v", r)
		if n, ok := node.(interface {
			Setattr(*fuse.SetattrRequest, *fuse.SetattrResponse, Intr) fuse.Error
		}); ok {
			if err := n.Setattr(r, s, intr); err != nil {
				done(err)
				r.RespondError(err)
				break
			}
			done(s)
			r.Respond(s)
			break
		}

		if s.AttrValid == 0 {
			s.AttrValid = 1 * time.Minute
		}
		s.Attr = snode.attr()
		done(s)
		r.Respond(s)

	case *fuse.SymlinkRequest:
		s := &fuse.SymlinkResponse{}
		n, ok := node.(interface {
			Symlink(*fuse.SymlinkRequest, Intr) (Node, fuse.Error)
		})
		if !ok {
			done(fuse.EIO) // XXX or EPERM like Mkdir?
			r.RespondError(fuse.EIO)
			break
		}
		n2, err := n.Symlink(r, intr)
		if err != nil {
			done(err)
			r.RespondError(err)
			break
		}
		c.saveLookup(&s.LookupResponse, snode, r.NewName, n2)
		done(s)
		r.Respond(s)

	case *fuse.ReadlinkRequest:
		n, ok := node.(interface {
			Readlink(*fuse.ReadlinkRequest, Intr) (string, fuse.Error)
		})
		if !ok {
			done(fuse.EIO) /// XXX or EPERM?
			r.RespondError(fuse.EIO)
			break
		}
		target, err := n.Readlink(r, intr)
		if err != nil {
			done(err)
			r.RespondError(err)
			break
		}
		done(target)
		r.Respond(target)

	case *fuse.LinkRequest:
		n, ok := node.(interface {
			Link(r *fuse.LinkRequest, old Node, intr Intr) (Node, fuse.Error)
		})
		if !ok {
			log.Printf("Node %T doesn't implement fuse Link", node)
			done(fuse.EIO) /// XXX or EPERM?
			r.RespondError(fuse.EIO)
			break
		}
		c.meta.Lock()
		var oldNode *serveNode
		if int(r.OldNode) < len(c.node) {
			oldNode = c.node[r.OldNode]
		}
		c.meta.Unlock()
		if oldNode == nil {
			log.Printf("In LinkRequest, node %d not found", r.OldNode)
			done(fuse.EIO)
			r.RespondError(fuse.EIO)
			break
		}
		n2, err := n.Link(r, oldNode.node, intr)
		if err != nil {
			done(err)
			r.RespondError(err)
			break
		}
		s := &fuse.LookupResponse{}
		c.saveLookup(s, snode, r.NewName, n2)
		done(s)
		r.Respond(s)

	case *fuse.RemoveRequest:
		n, ok := node.(interface {
			Remove(*fuse.RemoveRequest, Intr) fuse.Error
		})
		if !ok {
			done(fuse.EIO) /// XXX or EPERM?
			r.RespondError(fuse.EIO)
			break
		}
		err := n.Remove(r, intr)
		if err != nil {
			done(err)
			r.RespondError(err)
			break
		}
		done(nil)
		r.Respond()

	case *fuse.AccessRequest:
		if n, ok := node.(interface {
			Access(*fuse.AccessRequest, Intr) fuse.Error
		}); ok {
			if err := n.Access(r, intr); err != nil {
				done(err)
				r.RespondError(err)
				break
			}
		}
		done(r)
		r.Respond()

	case *fuse.LookupRequest:
		var n2 Node
		var err fuse.Error
		s := &fuse.LookupResponse{}
		if n, ok := node.(interface {
			Lookup(string, Intr) (Node, fuse.Error)
		}); ok {
			n2, err = n.Lookup(r.Name, intr)
		} else if n, ok := node.(interface {
			Lookup(*fuse.LookupRequest, *fuse.LookupResponse, Intr) (Node, fuse.Error)
		}); ok {
			n2, err = n.Lookup(r, s, intr)
		} else {
			done(fuse.ENOENT)
			r.RespondError(fuse.ENOENT)
			break
		}
		if err != nil {
			done(err)
			r.RespondError(err)
			break
		}
		c.saveLookup(s, snode, r.Name, n2)
		done(s)
		r.Respond(s)

	case *fuse.MkdirRequest:
		s := &fuse.MkdirResponse{}
		n, ok := node.(interface {
			Mkdir(*fuse.MkdirRequest, Intr) (Node, fuse.Error)
		})
		if !ok {
			done(fuse.EPERM)
			r.RespondError(fuse.EPERM)
			break
		}
		n2, err := n.Mkdir(r, intr)
		if err != nil {
			done(err)
			r.RespondError(err)
			break
		}
		c.saveLookup(&s.LookupResponse, snode, r.Name, n2)
		done(s)
		r.Respond(s)

	case *fuse.OpenRequest:
		s := &fuse.OpenResponse{Flags: fuse.OpenDirectIO}
		var h2 Handle
		if n, ok := node.(interface {
			Open(*fuse.OpenRequest, *fuse.OpenResponse, Intr) (Handle, fuse.Error)
		}); ok {
			hh, err := n.Open(r, s, intr)
			if err != nil {
				done(err)
				r.RespondError(err)
				break
			}
			h2 = hh
		} else {
			h2 = node
		}
		s.Handle, _ = c.saveHandle(h2, hdr.Node)
		done(s)
		r.Respond(s)

	case *fuse.CreateRequest:
		n, ok := node.(interface {
			Create(*fuse.CreateRequest, *fuse.CreateResponse, Intr) (Node, Handle, fuse.Error)
		})
		if !ok {
			// If we send back ENOSYS, FUSE will try mknod+open.
			done(fuse.EPERM)
			r.RespondError(fuse.EPERM)
			break
		}
		s := &fuse.CreateResponse{OpenResponse: fuse.OpenResponse{Flags: fuse.OpenDirectIO}}
		n2, h2, err := n.Create(r, s, intr)
		if err != nil {
			done(err)
			r.RespondError(err)
			break
		}
		c.saveLookup(&s.LookupResponse, snode, r.Name, n2)
		h, shandle := c.saveHandle(h2, hdr.Node)
		s.Handle = h
		type writeAll interface {
			WriteAll([]byte, Intr) fuse.Error
		}
		if _, ok := shandle.handle.(writeAll); ok {
			shandle.trunc = true
		}
		done(s)
		r.Respond(s)

	case *fuse.GetxattrRequest, *fuse.SetxattrRequest, *fuse.ListxattrRequest, *fuse.RemovexattrRequest:
		// TODO: Use n.
		done(fuse.ENOSYS)
		r.RespondError(fuse.ENOSYS)

	case *fuse.ForgetRequest:
		n, ok := node.(interface {
			Forget()
		})
		if ok {
			n.Forget()
		}
		c.dropNode(hdr.Node)
		done(r)
		r.Respond()

	// Handle operations.
	case *fuse.ReadRequest:
		shandle := c.getHandle(r.Handle)
		if shandle == nil {
			fuse.Debugf("-> %#x %v", hdr.ID, fuse.ESTALE)
			r.RespondError(fuse.ESTALE)
			return
		}
		handle := shandle.handle

		s := &fuse.ReadResponse{Data: make([]byte, 0, r.Size)}
		if r.Dir {
			if h, ok := handle.(interface {
				ReadDir(Intr) ([]fuse.Dirent, fuse.Error)
			}); ok {
				if shandle.readData == nil {
					dirs, err := h.ReadDir(intr)
					if err != nil {
						done(err)
						r.RespondError(err)
						break
					}
					var data []byte
					for _, dir := range dirs {
						if dir.Inode == 0 {
							dir.Inode = hash(path.Join(snode.name, dir.Name))
						}
						data = fuse.AppendDirent(data, dir)
					}
					shandle.readData = data
				}
				HandleRead(r, s, shandle.readData)
				done(s)
				r.Respond(s)
				break
			}
		} else {
			if h, ok := handle.(interface {
				ReadAll(Intr) ([]byte, fuse.Error)
			}); ok {
				if shandle.readData == nil {
					data, err := h.ReadAll(intr)
					if err != nil {
						done(err)
						r.RespondError(err)
						break
					}
					if data == nil {
						data = []byte{}
					}
					shandle.readData = data
				}
				HandleRead(r, s, shandle.readData)
				done(s)
				r.Respond(s)
				break
			}
			h, ok := handle.(interface {
				Read(*fuse.ReadRequest, *fuse.ReadResponse, Intr) fuse.Error
			})
			if !ok {
				fmt.Printf("NO READ FOR %T\n", handle)
				done(fuse.EIO)
				r.RespondError(fuse.EIO)
				break
			}
			if err := h.Read(r, s, intr); err != nil {
				done(err)
				r.RespondError(err)
				break
			}
		}
		done(s)
		r.Respond(s)

	case *fuse.WriteRequest:
		shandle := c.getHandle(r.Handle)
		if shandle == nil {
			fuse.Debugf("-> %#x %v", hdr.ID, fuse.ESTALE)
			r.RespondError(fuse.ESTALE)
			return
		}

		s := &fuse.WriteResponse{}
		if shandle.trunc && r.Offset == int64(len(shandle.writeData)) {
			shandle.writeData = append(shandle.writeData, r.Data...)
			s.Size = len(r.Data)
			done(s)
			r.Respond(s)
			break
		}
		if h, ok := shandle.handle.(interface {
			Write(*fuse.WriteRequest, *fuse.WriteResponse, Intr) fuse.Error
		}); ok {
			if err := h.Write(r, s, intr); err != nil {
				done(err)
				r.RespondError(err)
				break
			}
			done(s)
			r.Respond(s)
			break
		}
		println("NO WRITE")
		done(fuse.EIO)
		r.RespondError(fuse.EIO)

	case *fuse.FlushRequest:
		shandle := c.getHandle(r.Handle)
		if shandle == nil {
			fuse.Debugf("-> %#x %v", hdr.ID, fuse.ESTALE)
			r.RespondError(fuse.ESTALE)
			return
		}
		handle := shandle.handle

		if shandle.trunc {
			h := handle.(interface {
				WriteAll([]byte, Intr) fuse.Error
			})
			if err := h.WriteAll(shandle.writeData, intr); err != nil {
				done(err)
				r.RespondError(err)
				break
			}
			shandle.writeData = nil
			shandle.trunc = false
		}
		if h, ok := handle.(interface {
			Flush(*fuse.FlushRequest, Intr) fuse.Error
		}); ok {
			if err := h.Flush(r, intr); err != nil {
				done(err)
				r.RespondError(err)
				break
			}
		}
		done(nil)
		r.Respond()

	case *fuse.ReleaseRequest:
		shandle := c.getHandle(r.Handle)
		if shandle == nil {
			fuse.Debugf("-> %#x %v", hdr.ID, fuse.ESTALE)
			r.RespondError(fuse.ESTALE)
			return
		}
		handle := shandle.handle

		// No matter what, release the handle.
		c.dropHandle(r.Handle)

		if h, ok := handle.(interface {
			Release(*fuse.ReleaseRequest, Intr) fuse.Error
		}); ok {
			if err := h.Release(r, intr); err != nil {
				done(err)
				r.RespondError(err)
				break
			}
		}
		done(nil)
		r.Respond()

	case *fuse.DestroyRequest:
		fs, ok := fs.(interface {
			Destroy()
		})
		if ok {
			fs.Destroy()
		}
		done(nil)
		r.Respond()

	case *fuse.RenameRequest:
		c.meta.Lock()
		var newDirNode *serveNode
		if int(r.NewDir) < len(c.node) {
			newDirNode = c.node[r.NewDir]
		}
		c.meta.Unlock()
		if newDirNode == nil {
			println("RENAME NEW DIR NODE NOT FOUND")
			done(fuse.EIO)
			r.RespondError(fuse.EIO)
			break
		}
		n, ok := node.(interface {
			Rename(r *fuse.RenameRequest, newDir Node, intr Intr) fuse.Error
		})
		if !ok {
			log.Printf("Node %T missing Rename method", node)
			done(fuse.EIO) // XXX or EPERM like Mkdir?
			r.RespondError(fuse.EIO)
			break
		}
		err := n.Rename(r, newDirNode.node, intr)
		if err != nil {
			done(err)
			r.RespondError(err)
			break
		}
		done(nil)
		r.Respond()

	case *fuse.MknodRequest:
		n, ok := node.(interface {
			Mknod(r *fuse.MknodRequest, intr Intr) (Node, fuse.Error)
		})
		if !ok {
			log.Printf("Node %T missing Mknod method", node)
			done(fuse.EIO)
			r.RespondError(fuse.EIO)
			break
		}
		n2, err := n.Mknod(r, intr)
		if err != nil {
			done(err)
			r.RespondError(err)
			break
		}
		s := &fuse.LookupResponse{}
		c.saveLookup(s, snode, r.Name, n2)
		done(s)
		r.Respond(s)

	case *fuse.FsyncRequest:
		n, ok := node.(interface {
			Fsync(r *fuse.FsyncRequest, intr Intr) fuse.Error
		})
		if !ok {
			log.Printf("Node %T missing Fsync method", node)
			done(fuse.EIO)
			r.RespondError(fuse.EIO)
			break
		}
		err := n.Fsync(r, intr)
		if err != nil {
			done(err)
			r.RespondError(err)
			break
		}
		done(nil)
		r.Respond()

	case *fuse.InterruptRequest:
		c.meta.Lock()
		ireq := c.req[r.IntrID]
		if ireq != nil && ireq.Intr != nil {
			close(ireq.Intr)
			ireq.Intr = nil
		}
		c.meta.Unlock()
		done(nil)
		r.Respond()

		/*	case *FsyncdirRequest:
				done(ENOSYS)
				r.RespondError(ENOSYS)

			case *GetlkRequest, *SetlkRequest, *SetlkwRequest:
				done(ENOSYS)
				r.RespondError(ENOSYS)

			case *BmapRequest:
				done(ENOSYS)
				r.RespondError(ENOSYS)

			case *SetvolnameRequest, *GetxtimesRequest, *ExchangeRequest:
				done(ENOSYS)
				r.RespondError(ENOSYS)
		*/
	}
}

func (c *serveConn) saveLookup(s *fuse.LookupResponse, snode *serveNode, elem string, n2 Node) {
	name := path.Join(snode.name, elem)
	var sn *serveNode
	s.Node, s.Generation, sn = c.saveNode(name, n2)
	if s.EntryValid == 0 {
		s.EntryValid = 1 * time.Minute
	}
	if s.AttrValid == 0 {
		s.AttrValid = 1 * time.Minute
	}
	s.Attr = sn.attr()
}

// HandleRead handles a read request assuming that data is the entire file content.
// It adjusts the amount returned in resp according to req.Offset and req.Size.
func HandleRead(req *fuse.ReadRequest, resp *fuse.ReadResponse, data []byte) {
	if req.Offset >= int64(len(data)) {
		data = nil
	} else {
		data = data[req.Offset:]
	}
	if len(data) > req.Size {
		data = data[:req.Size]
	}
	n := copy(resp.Data[:req.Size], data)
	resp.Data = resp.Data[:n]
}

// DataHandle returns a read-only Handle that satisfies reads
// using the given data.
func DataHandle(data []byte) Handle {
	return &dataHandle{data}
}

type dataHandle struct {
	data []byte
}

func (d *dataHandle) ReadAll(intr Intr) ([]byte, fuse.Error) {
	return d.data, nil
}
