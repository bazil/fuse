package fuse

//lint:ignore U1000 Used by TestMountOptionCommaError to bypass safe API

// for TestMountOptionCommaError
func ForTestSetMountOption(k, v string) MountOption {
	fn := func(conf *MountConfig) error {
		conf.options[k] = v
		return nil
	}
	return fn
}
