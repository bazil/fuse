// +build !darwin
// +build !freebsd

package fuse

func allowRoot(conf *MountConfig) error {
	if _, ok := conf.options["allow_other"]; ok {
		return ErrCannotCombineAllowOtherAndAllowRoot
	}
	conf.options["allow_root"] = ""
	return nil
}
