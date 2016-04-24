package syscallx

type Fusefs_args struct {
	Name    string
	FD      int
	MaxRead int
}
