//go:build gofuzz

package mountinfo

func Fuzz(data []byte) int {
	if _, err := parse(data); err != nil {
		return 0
	}
	return 1
}
