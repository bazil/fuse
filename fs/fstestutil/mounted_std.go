// +build !darwin

package fstestutil

import (
	"testing"
)

func waitForMount(t testing.TB, dir string, abort <-chan struct{}) {}
