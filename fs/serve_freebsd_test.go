package fs_test

import (
	"os"
	"syscall"
)

func platformStatfs(st *syscall.Statfs_t) *statfsResult {
	return &statfsResult{
		Blocks:  st.Blocks,
		Bfree:   st.Bfree,
		Bavail:  uint64(st.Bavail),
		Files:   st.Files,
		Ffree:   uint64(st.Ffree),
		Bsize:   int64(st.Iosize),
		Namelen: int64(st.Namemax),
		Frsize:  int64(st.Bsize),
	}
}

func platformStat(fi os.FileInfo) *statResult {
	r := &statResult{
		Mode: fi.Mode(),
	}
	st := fi.Sys().(*syscall.Stat_t)
	r.Ino = st.Ino
	r.Nlink = st.Nlink
	r.UID = st.Uid
	r.GID = st.Gid
	r.Blksize = int64(st.Blksize)
	return r
}
