//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package controller

import "github.com/dkropachev/agent-workflow/internal/ownership"

type Lock = ownership.Lock

var Own = ownership.Own
