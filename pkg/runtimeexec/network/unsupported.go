//go:build !linux

package network

import (
	"errors"
	"net"
	"os"
)

func trustedConnection(*net.UnixConn) error {
	return errors.New("network runtime requires Linux/rootful Podman")
}

func Prepare(name string, uid uint32) error {
	return errors.New("network runtime requires Linux/rootful Podman")
}
func PrepareProtected(name string, uid uint32, enable bool, authentication []string) error {
	return errors.New("network runtime requires Linux/BPF LSM/rootful Podman")
}
func Start(name string) error { return errors.New("network runtime requires Linux/rootful Podman") }

func sealedPayload(any) (*os.File, error) { return nil, errors.New("network runtime requires Linux") }
