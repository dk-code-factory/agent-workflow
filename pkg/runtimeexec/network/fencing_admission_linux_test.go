//go:build linux

package network

import (
	"os"
	"strings"
	"testing"
)

func TestFencingCapabilityRequiredOnlyForNewAdmissions(t *testing.T) {
	s, socket := fixtureService(t)
	ref := s.status.Reference
	id := randomID()
	s.mu.Lock()
	s.status.Capabilities = nil // Status retained by a pre-fencing service.
	s.status.Members[id] = Membership{ID: id, UID: uint32(os.Getuid()), State: "reserved"}
	s.mu.Unlock()
	_, status := requestAt(t, socket, Request{Operation: "status"}, false)
	if err := status.Status.admissionReady(); err == nil || !strings.Contains(err.Error(), ContainerAPIFencing) {
		t.Fatalf("old service admitted: %v", err)
	}
	for _, op := range []string{"check", "reserve", "exec", "forward"} {
		_, out := requestAt(t, socket, Request{Operation: op, Reference: ref, ID: randomID(), Path: "/bin/true"}, op == "exec")
		if !strings.Contains(out.Error, ContainerAPIFencing) {
			t.Fatalf("%s bypassed capability requirement: %+v", op, out)
		}
	}
	for _, op := range []string{"cleanup-check", "member", "release"} {
		_, out := requestAt(t, socket, Request{Operation: op, Reference: ref, ID: id}, false)
		if out.Error != "" {
			t.Fatalf("old service cannot perform retained %s: %s", op, out.Error)
		}
	}
	stale := ref
	stale.Generation++
	_, out := requestAt(t, socket, Request{Operation: "cleanup-check", Reference: stale}, false)
	if out.Error == "" {
		t.Fatal("cleanup accepted changed generation")
	}
}

func TestStatusCapabilityCloneDoesNotAliasService(t *testing.T) {
	s, _ := fixtureService(t)
	copy := cloneStatus(s.status)
	copy.Capabilities[0] = "changed"
	if s.status.Capabilities[0] != ContainerAPIFencing {
		t.Fatal("status consumer mutated admission capability")
	}
}
