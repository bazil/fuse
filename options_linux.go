package fuse

func localVolume(conf *mountConfig) error {
	return nil
}

func volumeName(name string) MountOption {
	return dummyOption
}

func daemonTimeout(name string) MountOption {
	return dummyOption
}
