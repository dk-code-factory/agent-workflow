//go:build !linux

package network

import (
	"errors"
	"net"
)

func trustedConnection(*net.UnixConn) error {
	return errors.New("network runtime requires Linux/rootful Podman")
}

func Prepare(name string, uid uint32) error {
	return errors.New("network runtime requires Linux/rootful Podman")
}
func Start(name string) error { return errors.New("network runtime requires Linux/rootful Podman") }
