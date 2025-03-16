package fuse

func daemonTimeout(name string) MountOption {
	return func(conf *MountConfig) error {
		conf.options["timeout"] = name
		return nil
	}
}
