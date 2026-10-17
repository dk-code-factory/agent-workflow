//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package ownership

import "fmt"

type Lock struct{}

func (*Lock) Holds(string) bool { return false }

func Own(string) (*Lock, error) { return nil, fmt.Errorf("controller requires a supported Unix host") }
func (*Lock) Close() error      { return nil }
