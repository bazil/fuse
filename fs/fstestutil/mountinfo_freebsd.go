package fstestutil

import (
  "syscall"
)

// cstr converts a nil-terminated C string into a Go string
func cstr(ca []int8) string {
	s := make([]byte, 0, len(ca))
	for _, c := range ca {
		if c == 0x00 {
			break
		}
		s = append(s, byte(c))
	}
	return string(s)
}

func getMountInfo(mnt string) (*MountInfo, error) {
  var st syscall.Statfs_t
  err := syscall.Statfs(mnt, &st)
  if err != nil { return nil, err }
  return &MountInfo{FSName: cstr(st.Mntfromname[:])}, nil
}
