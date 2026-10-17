//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package controller

import "github.com/dk-code-factory/agent-workflow/internal/ownership"

type Lock = ownership.Lock

var Own = ownership.Own
