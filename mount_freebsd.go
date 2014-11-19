package fuse

import (
	"fmt"
	"os"
	"os/exec"
)

func mount(dir string, conf *MountConfig, ready chan<- struct{}, errp *error) (*os.File, error) {

	f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0000)
	if err != nil {
		*errp = err
		return nil, err
	}

	cmd := exec.Command("/sbin/mount_fusefs",
		"-o", conf.getOptions(),
		"3",
		dir)
	cmd.ExtraFiles = []*os.File{f}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("mount_fusefs: %q, %v", out, err)
	}
	close(ready)
	return f, nil
}
