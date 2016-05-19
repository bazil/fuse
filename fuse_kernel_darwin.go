package fuse

type attr struct {
	Ino        uint64
	Size       uint64
	Blocks     uint64
	Atime      uint64
	Mtime      uint64
	Ctime      uint64
	Crtime_    uint64 // OS X only
	AtimeNsec  uint32
	MtimeNsec  uint32
	CtimeNsec  uint32
	CrtimeNsec uint32 // OS X only
	Mode       uint32
	Nlink      uint32
	Uid        uint32
	Gid        uint32
	Rdev       uint32
	Flags_     uint32 // OS X only; see chflags(2)
	Blksize    uint32
	padding    uint32
}

func (a *attr) SetCrtime(s uint64, ns uint32) {
	a.Crtime_, a.CrtimeNsec = s, ns
}

func (a *attr) SetFlags(f uint32) {
	a.Flags_ = f
}

func openFlags(flags uint32) OpenFlags {
	return OpenFlags(flags)
}
