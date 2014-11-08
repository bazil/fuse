package fuse

func localVolume(conf *MountConfig) error {
	conf.options["local"] = ""
	return nil
}
