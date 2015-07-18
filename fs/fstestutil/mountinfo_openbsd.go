package fstestutil

import "errors"

func getMountInfo(mnt string) (*MountInfo, error) {
	return nil, errors.New("OpenBSD has no useful mount information")
}
