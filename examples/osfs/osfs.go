/*
Package osfs implements a file system that functions as a pass-through
to the os file system. Not all features are supported.
*/
package osfs // import "bazil.org/fuse/examples/osfs"

import (
	"io/ioutil"
	"os"
	"syscall"
	"time"
	"path/filepath"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

// New returns a new pass-through fs.FS.
//
// Not optimized at all.
func New(path string) fs.FS {
	return &filesystem{path}
}

type filesystem struct {
	path string
}

func (f *filesystem) Root() (fs.Node, error) {
	return &dir{node{f.path}}, nil
}

type node struct {
	path string
}

func (n *node) Attr(ctx context.Context, attr *fuse.Attr) (retErr error) {
	stat, err := os.Stat(n.path)
	if err != nil {
		return err
	}
	attr.Size = uint64(stat.Size())
	attr.Mode = stat.Mode()
	attr.Mtime = stat.ModTime()
	if st, ok := stat.Sys().(*syscall.Stat_t); ok {
		attr.Inode = st.Ino
		attr.Blocks = uint64(st.Blocks)
		attr.Atime = time.Unix(st.Atim.Unix())
		attr.Ctime = time.Unix(st.Ctim.Unix())
		attr.Nlink = uint32(st.Nlink)
		attr.Uid = st.Uid
		attr.Gid = st.Gid
		attr.Rdev = uint32(st.Rdev)
		attr.BlockSize = uint32(st.Blksize)
	}
	return nil
}

func (n *node) Setattr(ctx context.Context, request *fuse.SetattrRequest, response *fuse.SetattrResponse) (retErr error) {
	if request.Valid.Uid() || request.Valid.Gid() {
		uid, gid := -1, -1
		if request.Valid.Uid() {
			uid = int(request.Uid)
		}
		if request.Valid.Gid() {
			gid = int(request.Gid)
		}
		if err := os.Chown(n.path, uid, gid); err != nil {
			return err
		}
	}
	if request.Valid.Mode() {
		if err := os.Chmod(n.path, request.Mode); err != nil {
			return err
		}
	}
	if request.Valid.Size() {
		if err := os.Truncate(n.path, int64(request.Size)); err != nil {
			return err
		}
	}
	if request.Valid.Mtime() || request.Valid.Atime() {
		var mtime, atime time.Time
		if !request.Valid.Mtime() || !request.Valid.Atime() {
			stat, err := os.Stat(n.path)
			if err != nil {
				return err
			}
			mtime = stat.ModTime()
			if st, ok := stat.Sys().(*syscall.Stat_t); ok {
				atime = time.Unix(st.Atim.Unix())
			} else {
				atime = time.Now()
			}
		}
		if request.Valid.Mtime() {
			mtime = request.Mtime
		}
		if request.Valid.Atime() {
			atime = request.Atime
		}
		if err := os.Chtimes(n.path, mtime, atime); err != nil {
			return err
		}
	}
	return nil
}

type dir struct {
	node
}

func (d *dir) Create(ctx context.Context, request *fuse.CreateRequest, response *fuse.CreateResponse) (_ fs.Node, _ fs.Handle, retErr error) {
	file := &file{node{filepath.Join(d.path, request.Name)}}
	handle, err := file.openFile(int(request.Flags), request.Mode)
	return file, handle, err
}

func (d *dir) Lookup(ctx context.Context, name string) (_ fs.Node, retErr error) {
	path := filepath.Join(d.path, name)
	stat, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fuse.ENOENT
		}
		return nil, err
	}
	if stat.IsDir() {
		return &dir{node{path}}, nil
	}
	return &file{node{path}}, nil
}

func (d *dir) Mkdir(ctx context.Context, request *fuse.MkdirRequest) (_ fs.Node, retErr error) {
	path := filepath.Join(d.path, request.Name)
	if err := os.Mkdir(path, request.Mode); err != nil {
		return nil, err
	}
	return &dir{node{path}}, nil
}

func (d *dir) ReadDirAll(ctx context.Context) (_ []fuse.Dirent, retErr error) {
	stats, err := ioutil.ReadDir(d.path)
	if err != nil {
		return nil, err
	}
	dirents := make([]fuse.Dirent, len(stats))
	for i, stat := range stats {
		t := fuse.DT_Unknown
		switch stat.Mode() & os.ModeType {
		case os.ModeDir:
			t = fuse.DT_Dir
		case os.ModeSymlink:
			t = fuse.DT_Link
		case os.ModeNamedPipe:
			t = fuse.DT_FIFO
		case os.ModeSocket:
			t = fuse.DT_Socket
		case os.ModeDevice:
			if stat.Mode() & os.ModeCharDevice == 0 {
				t = fuse.DT_Block
			} else {
				t = fuse.DT_Char
			}
		case 0:
			t = fuse.DT_File
		}
		dirents[i] = fuse.Dirent{
			Name: filepath.Base(stat.Name()),
			Type: t,
		}
	}
	return dirents, nil
}

func (d *dir) Remove(ctx context.Context, request *fuse.RemoveRequest) error {
	return os.Remove(filepath.Join(d.path, request.Name))
}

type file struct {
	node
}

func (f *file) Open(ctx context.Context, request *fuse.OpenRequest, response *fuse.OpenResponse) (_ fs.Handle, retErr error) {
	return f.openFile(int(request.Flags), 0666)
}

func (f *file) openFile(flags int, mode os.FileMode) (fs.Handle, error) {
	file, err := os.OpenFile(f.path, flags, mode)
	if err != nil {
		return nil, err
	}
	return &handle{f.node, file}, nil
}

type handle struct {
	node
	f *os.File
}

func (h *handle) Read(ctx context.Context, request *fuse.ReadRequest, response *fuse.ReadResponse) (retErr error) {
	if _, err := h.f.Seek(request.Offset, 0); err != nil {
		return err
	}
	buffer := make([]byte, request.Size)
	n, err := h.f.Read(buffer)
	response.Data = buffer[:n]
	if err != nil {
		return err
	}
	return nil
}

func (h *handle) Write(ctx context.Context, request *fuse.WriteRequest, response *fuse.WriteResponse) (retErr error) {
	if _, err := h.f.Seek(request.Offset, 0); err != nil {
		return err
	}
	n, err := h.f.Write(request.Data)
	response.Size = n
	if err != nil {
		return err
	}
	return nil
}

func (h *handle) Fsync(ctx context.Context, request *fuse.FsyncRequest) error {
	return nil
}

func (h *handle) Release(ctx context.Context, request *fuse.ReleaseRequest) error {
	return h.f.Close()
}
