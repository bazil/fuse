package fuse

// Unmount tries to unmount the filesystem mounted at dir.
func Unmount(dir string, options ...MountOption) error {
	conf := MountConfig{
		options: make(map[string]string),
	}
	for _, option := range options {
		if err := option(&conf); err != nil {
			return err
		}
	}

	if conf.umountFunc != nil {
		return conf.umountFunc(dir)
	}

	return unmount(dir)
}
