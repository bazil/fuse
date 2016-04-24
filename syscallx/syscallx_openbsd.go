package syscallx

import "errors"

func Getxattr(path string, attr string, dest []byte) (sz int, err error) {
	return 0, errors.New("not implemented")
}

func Listxattr(path string, dest []byte) (sz int, err error) {
	return 0, errors.New("not implemented")
}

func Setxattr(path string, attr string, data []byte, flags int) (err error) {
	return errors.New("not implemented")
}

func Removexattr(path string, attr string) (err error) {
	return errors.New("not implemented")
}
