package fs_test

import (
	"os"
	"syscall"
)

func platformStatfs(st *syscall.Statfs_t) *statfsResult {
	return &statfsResult{
		Blocks:  st.Blocks,
		Bfree:   st.Bfree,
		Bavail:  st.Bavail,
		Files:   st.Files,
		Ffree:   st.Ffree,
		Bsize:   st.Bsize,
		Namelen: st.Namelen,
		Frsize:  st.Frsize,
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
	r.Blksize = st.Blksize
	return r
}
