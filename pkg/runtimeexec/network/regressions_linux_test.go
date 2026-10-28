//go:build linux

package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHistoryRepliesBoundedWithoutDiscardingOwnership(t *testing.T) {
	s, socket := fixtureService(t)
	s.mu.Lock()
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("%032x", i)
		s.status.Members[id] = Membership{ID: id, UID: uint32(os.Getuid()), State: "cleaned"}
	}
	uncertain := strings.Repeat("f", 32)
	s.status.Members[uncertain] = Membership{ID: uncertain, UID: uint32(os.Getuid()), State: "uncertain"}
	for i := 0; i < 10000; i++ {
		s.status.Forwards = append(s.status.Forwards, Forward{ID: i, HostPort: i + 1, GuestPort: 80})
	}
	if err := save(filepath.Join(s.dir, "status.json"), s.status); err != nil {
		t.Fatal(err)
	}
	ref := s.status.Reference
	s.mu.Unlock()
	var previous []byte
	for i := 0; i < 2; i++ {
		c, out := requestAt(t, socket, Request{Operation: "status"}, false)
		c.Close()
		encoded, _ := json.Marshal(out)
		if out.Error != "" || len(encoded) > 64<<10 || out.Status.MemberCount != 10001 || out.Status.ForwardCount != 10000 || !out.Status.MembersTruncated || !out.Status.ForwardsTruncated || len(out.Status.Members) != 128 || len(out.Status.Forwards) != 128 || out.Status.Members[uncertain].State != "uncertain" {
			t.Fatalf("invalid bounded preview: %d bytes %+v", len(encoded), out.Status)
		}
		if previous != nil && string(previous) != string(encoded) {
			t.Fatal("preview changed without state change")
		}
		previous = encoded
	}
	c, check := requestAt(t, socket, Request{Operation: "check", Reference: ref}, false)
	c.Close()
	if check.Error != "" || !check.Status.Healthy || len(check.Status.Members) != 0 || len(check.Status.Forwards) != 0 || check.Status.MemberCount != 10001 || check.Status.ForwardCount != 10000 {
		t.Fatal(check)
	}
	for _, id := range []string{fmt.Sprintf("%032x", 9999), uncertain} {
		c, member := requestAt(t, socket, Request{Operation: "member", Reference: ref, ID: id}, false)
		c.Close()
		if member.Error != "" || member.Member.ID != id {
			t.Fatal(member)
		}
	}
	c, stop := requestAt(t, socket, Request{Operation: "stop", Reference: ref}, false)
	c.Close()
	if stop.Error == "" {
		t.Fatal("truncated uncertain member did not block stop")
	}
	var durable Status
	if err := load(filepath.Join(s.dir, "status.json"), &durable); err != nil || len(durable.Members) != 10001 || len(durable.Forwards) != 10000 {
		t.Fatalf("durable state lost: %v", err)
	}
}

func TestConcurrentAndRepeatedStop(t *testing.T) {
	s, socket := fixtureService(t)
	req := Request{Operation: "stop", Reference: s.status.Reference}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, out := requestAt(t, socket, req, false)
			c.Close()
			if out.Error != "" {
				t.Error(out.Error)
			}
		}()
	}
	wg.Wait()
	select {
	case <-s.stop:
	default:
		t.Fatal("stop signal not delivered")
	}
	c, out := requestAt(t, socket, req, false)
	c.Close()
	if out.Error != "" {
		t.Fatal(out.Error)
	}
}

func TestPreStartFailureFinalizationAndPersistenceFailure(t *testing.T) {
	for _, stage := range []string{"authority", "start"} {
		for _, saveFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/save-fails=%t", stage, saveFails), func(t *testing.T) {
				s, socket := fixtureService(t)
				setupErr := errors.New("peer vanished before authority read")
				s.authority = func(cred *unix.Ucred) ([]uint32, int, error) {
					if saveFails {
						if err := os.Mkdir(filepath.Join(s.dir, "status.json.partial"), 0700); err != nil {
							t.Error(err)
						}
					}
					if stage == "authority" {
						return nil, 0, setupErr
					}
					return peerAuthority(cred)
				}
				if stage == "start" {
					s.memberCommand = func(string) *exec.Cmd { return exec.Command(filepath.Join(t.TempDir(), "missing-member")) }
				}
				id := randomID()
				ref := s.status.Reference
				c, out := requestAt(t, socket, Request{Operation: "exec", Reference: ref, ID: id, Path: "/bin/true"}, true)
				c.Close()
				if out.Error == "" {
					t.Fatal("setup failure accepted")
				}
				expected := "cleaned"
				if saveFails {
					expected = "launching"
					if !strings.Contains(out.Error, "cleanup persistence unconfirmed") {
						t.Fatal(out.Error)
					}
				}
				c, out = requestAt(t, socket, Request{Operation: "member", Reference: ref, ID: id}, false)
				c.Close()
				if out.Member.State != expected {
					t.Fatal(out)
				}
				var durable Status
				if err := load(filepath.Join(s.dir, "status.json"), &durable); err != nil || durable.Members[id].State != expected {
					t.Fatalf("durable member %+v, %v", durable.Members[id], err)
				}
				c, out = requestAt(t, socket, Request{Operation: "stop", Reference: ref}, false)
				c.Close()
				if (out.Error != "") != saveFails {
					t.Fatalf("stop result: %+v", out)
				}
			})
		}
	}
}

func TestPeerAuthorityEmptyGroupsAndMalformedMetadata(t *testing.T) {
	cred := &unix.Ucred{Uid: 1000, Gid: 1001}
	prefix := "Uid:\t1000 1000 1000 1000\nGid:\t1001 1001 1001 1001\nUmask:\t0027\n"
	groups, mask, err := parsePeerAuthority([]byte(prefix+"Groups:\t\n"), cred)
	if err != nil || len(groups) != 0 || mask != 0027 {
		t.Fatalf("empty groups rejected: %v %o %v", groups, mask, err)
	}
	for _, metadata := range []string{prefix, prefix + "Groups: nope\n", strings.Replace(prefix, "Umask:\t0027", "Umask:", 1) + "Groups:\n", strings.Replace(prefix, "1000 1000", "1000 2000", 1) + "Groups:\n"} {
		if _, _, err := parsePeerAuthority([]byte(metadata), cred); err == nil {
			t.Fatalf("malformed authority accepted: %q", metadata)
		}
	}
}

func TestDescriptorLaunchExceedsPacketSize(t *testing.T) {
	s, socket := fixtureService(t)
	s.memberCommand = func(string) *exec.Cmd { return exec.Command("/bin/false") }
	s.authority = func(c *unix.Ucred) ([]uint32, int, error) { return peerAuthority(c) }
	// Capture the production launch configuration through a helper command; its
	// receipt confirms that the service decoded all bytes from the private fd.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.memberCommand = func(string) *exec.Cmd {
		return exec.Command(binary, "-test.run=^TestLargePayloadMemberFixture$", "--", "large-payload")
	}
	req := Request{Operation: "exec", Reference: s.status.Reference, ID: randomID(), Path: "/bin/true", Args: []string{strings.Repeat("a", 70000), strings.Repeat("b", 70000)}}
	for i := 0; i < 200; i++ {
		req.Env = append(req.Env, fmt.Sprintf("VALUE_%d=%s", i, strings.Repeat("v", 1024)))
	}
	c, out := requestAt(t, socket, req, true)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	out = readResponse(t, c)
	c.Close()
	if out.Error != "" || out.Member.State != "cleaned" || out.Member.ID != req.ID {
		t.Fatal(out)
	}
}
func TestLargePayloadMemberFixture(t *testing.T) {
	if os.Args[len(os.Args)-1] != "large-payload" {
		return
	}
	var launch memberLaunch
	if err := decodePayload(os.NewFile(4, "launch"), &launch); err != nil {
		os.Exit(125)
	}
	req := launch.Request
	if len(req.Args) != 2 || len(req.Args[0]) != 70000 || len(req.Args[1]) != 70000 || len(req.Env) != 200 || len(req.Env[199]) < 1024 {
		os.Exit(125)
	}
	if save(launch.Receipt, Membership{ID: req.ID, UID: launch.UID, State: "cleaned"}) != nil {
		os.Exit(125)
	}
	os.Exit(0)
}

func TestCompletionReceiptUsesCleanAckIndependentlyOfExit125(t *testing.T) {
	exit := exec.Command("/bin/sh", "-c", "exit 125").Run()
	for _, ack := range []string{"\x01", "", "\x00", "\x01\x01"} {
		launch := memberLaunch{Request: Request{ID: randomID()}, UID: uint32(os.Getuid()), Receipt: filepath.Join(t.TempDir(), "receipt")}
		if _, err := completeMember(launch, exit, strings.NewReader(ack)); err != nil {
			t.Fatal(err)
		}
		var receipt Membership
		if err := load(launch.Receipt, &receipt); err != nil {
			t.Fatal(err)
		}
		expected := "uncertain"
		if ack == "\x01" {
			expected = "cleaned"
		}
		if receipt.State != expected || receipt.ExitCode != 125 {
			t.Fatal(receipt)
		}
	}
}

func TestSealedPayloadRejectsOpenPipesAndIgnoresSharedOffset(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := r.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// The writer stays open: rejection must not attempt to read to EOF.
	if err := decodeSealedPayload(r, new(Request)); err == nil {
		t.Fatal("unsealed pipe accepted")
	}
	payload, err := sealedPayload(Request{ID: "preserved"})
	if err != nil {
		t.Fatal(err)
	}
	defer payload.Close()
	if _, err := payload.Seek(0, 2); err != nil {
		t.Fatal(err)
	}
	var req Request
	if err := decodeSealedPayload(payload, &req); err != nil || req.ID != "preserved" {
		t.Fatalf("shared offset influenced request: %+v %v", req, err)
	}
}
