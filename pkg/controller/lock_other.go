//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package controller

import "fmt"

type Lock struct{}

func Own(string) (*Lock, error) { return nil, fmt.Errorf("controller requires a supported Unix host") }
func (*Lock) Close() error      { return nil }
