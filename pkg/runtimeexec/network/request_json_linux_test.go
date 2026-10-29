//go:build linux

package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRequestBytesAcrossDirectNestedAndRepeatedJSON(t *testing.T) {
	ref := Reference{Name: "test", Identity: strings.Repeat("a", 32), Generation: 9}
	ref.Endpoint = Endpoint(ref.Name, ref.Identity)
	want := Request{Reference: ref, Operation: "exec", ID: randomID(), Path: "/program-\xff", Dir: "/cwd-\xfe", Args: []string{"", "UTF8-é", "\ufffd", "\x00", "arg-\xfd"}, Env: []string{"", "A=\xfc", "A=last"}, Umask: 0027, Terminal: true, Forward: Forward{HostPort: 1234}}
	got := want
	for i := 0; i < 4; i++ {
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{`"path":`, `"args":`, `"env":`, `"dir":`} {
			if strings.Contains(string(data), field) {
				t.Fatal("legacy launch field emitted", field)
			}
		}
		if !strings.Contains(string(data), `"byte_encoding":"base64-v1"`) {
			t.Fatal("missing byte encoding")
		}
		if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip %d: %+v %v", i, got, err)
		}
		nested, err := json.Marshal(memberLaunch{Request: got, UID: 1234})
		if err != nil {
			t.Fatal(err)
		}
		var launch memberLaunch
		if err := json.NewDecoder(strings.NewReader(string(nested))).Decode(&launch); err != nil || !reflect.DeepEqual(launch.Request, want) {
			t.Fatalf("nested round trip: %+v %v", launch.Request, err)
		}
		got = launch.Request
	}
	for _, control := range []Request{{Operation: "status"}, {Operation: "exec", ID: randomID()}} {
		data, err := json.Marshal(control)
		if err != nil {
			t.Fatal(err)
		}
		legacy, _ := json.Marshal(requestFields(control))
		if string(data) != string(legacy) {
			t.Fatal("control encoding changed")
		}
	}
	legacy := Request{Operation: "exec", Path: "/UTF8-é", Args: []string{"", "é"}, Env: []string{"", "VALUE=é"}, Dir: "/cwd"}
	data, _ := json.Marshal(requestFields(legacy))
	if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, legacy) {
		t.Fatalf("legacy compatibility: %+v %v", got, err)
	}
	for _, outer := range [][]string{nil, {}} {
		data, _ := json.Marshal(Request{Path: "/program", Args: outer, Env: outer})
		if err := json.Unmarshal(data, &got); err != nil || got.Args != nil || got.Env != nil {
			t.Fatal("omitempty outer slice behavior changed", err)
		}
	}
	// Existing JSON field matching and last duplicate precedence still apply.
	if err := json.Unmarshal([]byte(`{"byte_encoding":"old","BYTE_ENCODING":"base64-v1","path_bytes":"YQ==","PATH_BYTES":"Yg==","args_bytes":["",null]}`), &got); err != nil || got.Path != "b" || !reflect.DeepEqual(got.Args, []string{"", ""}) {
		t.Fatalf("standard field matching: %+v %v", got, err)
	}
	if err := json.Unmarshal([]byte(`{"path":"first","PATH":"last"}`), &got); err != nil || got.Path != "last" {
		t.Fatal("legacy field matching changed", err)
	}
}

func TestRequestBytesRejectMalformedWithoutChangingReceiver(t *testing.T) {
	malformed := []string{
		`{"byte_encoding":null}`, `{"byte_encoding":""}`, `{"byte_encoding":1}`, `{"byte_encoding":[]}`, `{"byte_encoding":{}}`, `{"byte_encoding":"secret-unsupported-version"}`,
		`{"path_bytes":null}`, `{"args_bytes":[]}`, `{"env_bytes":""}`, `{"dir_bytes":null}`,
		`{"byte_encoding":"base64-v1","path":"legacy-secret","path_bytes":"YQ=="}`,
		`{"byte_encoding":"base64-v1","args":["legacy-secret"]}`, `{"byte_encoding":"base64-v1","env":["SECRET=legacy"]}`, `{"byte_encoding":"base64-v1","dir":"legacy-secret"}`,
		`{"byte_encoding":"base64-v1","path_bytes":"secret-invalid-base64"}`,
		`{"byte_encoding":"base64-v1","path_bytes":[65]}`, `{"byte_encoding":"base64-v1","env_bytes":[[65]]}`, `{"byte_encoding":"base64-v1","args_bytes":"YQ=="}`, `{"byte_encoding":"base64-v1","dir_bytes":123}`,
	}
	for _, field := range []string{"path_bytes", "args_bytes", "env_bytes", "dir_bytes"} {
		for _, empty := range []string{`null`, `""`, `[]`} {
			malformed = append(malformed, fmt.Sprintf(`{"%s":%s}`, field, empty))
		}
	}
	for _, data := range malformed {
		before := Request{Path: "untouched"}
		got := before
		err := json.Unmarshal([]byte(data), &got)
		if !errors.Is(err, errRequestEncoding) || !reflect.DeepEqual(got, before) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("codec rejection/atomicity: %v %+v", err, got)
		}
	}
}

func TestManySmallEnvironmentEntriesPassNativeAndService(t *testing.T) {
	env := manySmallEnvironment()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	native := exec.CommandContext(ctx, "/bin/true")
	native.Env = env
	native.WaitDelay = time.Second
	if out, err := native.CombinedOutput(); err != nil {
		t.Fatalf("native environment: %s %v", out, err)
	}
	s, socket := fixtureService(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.memberCommand = func(string) *exec.Cmd {
		return exec.Command(binary, "-test.run=^TestManyEnvironmentMemberFixture$", "--", "env-count")
	}
	req := Request{Operation: "exec", Reference: s.status.Reference, ID: randomID(), Path: "/bin/true", Env: Environment(env, s.status.Reference)}
	c, out := requestAt(t, socket, req, true)
	defer c.Close()
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	out = readResponse(t, c)
	if out.Error != "" || out.Member.State != "cleaned" || out.Member.ID != req.ID || out.Member.ExitCode != 0 {
		t.Fatal(out)
	}
	var receipt Membership
	if err := load(filepath.Join(s.dir, "member-"+req.ID+".done"), &receipt); err != nil || receipt.State != "cleaned" {
		t.Fatalf("durable receipt: %+v %v", receipt, err)
	}
}
func manySmallEnvironment() []string {
	env := make([]string, 10001)
	for i := range env {
		env[i] = fmt.Sprintf("FAKE_%05d=é", i)
	}
	return env
}
func TestManyEnvironmentMemberFixture(t *testing.T) {
	if os.Args[len(os.Args)-1] != "env-count" {
		return
	}
	var launch memberLaunch
	if decodeSealedPayload(os.NewFile(4, "launch"), &launch) != nil || !reflect.DeepEqual(launch.Request.Env, Environment(manySmallEnvironment(), launch.Request.Reference)) {
		os.Exit(125)
	}
	if save(launch.Receipt, Membership{ID: launch.Request.ID, UID: launch.UID, State: "cleaned"}) != nil {
		os.Exit(125)
	}
	os.Exit(0)
}

func TestRequestCodecRetainsFiniteSealedPayloadLimit(t *testing.T) {
	fd, err := unix.MemfdCreate("oversize-request-fixture", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "oversize-request-fixture")
	defer f.Close()
	// Sparse, disposable memory descriptor: no shared filesystem is exhausted.
	if err := unix.Ftruncate(fd, maxLaunchPayload+1); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		t.Fatal(err)
	}
	if err := decodeSealedPayload(f, new(Request)); err == nil {
		t.Fatal("oversize sealed payload accepted")
	}
}
