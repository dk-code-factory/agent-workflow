//go:build linux

package network

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

// Routing projections are computed with the launch caller's credentials. The
// privileged mount helper consumes only this data, never the caller's config.
// Directory seals prevent creating a later drop-in/context to undo a projection.
type routingPlan struct {
	Files        []routingFile
	Sockets      []string
	ReadOnlyDirs []string
	EmptyDirs    []string
}
type routingFile struct {
	Path string
	Data []byte
	Mode os.FileMode
}

const maxRoutingFile = 8 << 20
const maxRoutingEntries = 2048
const runtimeConnectionName = "agent-workflow-runtime"

type routingPlanner struct {
	plan     routingPlan
	files    map[string]string
	sockets  map[string]bool
	active   string
	endpoint string
	total    int
}

func planContainerRouting(env []string, ref Reference, uid uint32) (routingPlan, error) {
	return planContainerRoutingPaths(env, ref, uid, []string{"/usr/share/containers", "/etc/containers"})
}

// The explicit system directories let deterministic fixtures exercise the same
// planner without depending on the machine's installed container tools.
func planContainerRoutingPaths(env []string, ref Reference, uid uint32, systemDirs []string) (routingPlan, error) {
	if err := ref.Validate(); err != nil {
		return routingPlan{}, err
	}
	if uint32(os.Geteuid()) != uid {
		return routingPlan{}, errors.New("routing configuration must be read as the authenticated caller")
	}
	p := &routingPlanner{files: map[string]string{}, sockets: map[string]bool{}, endpoint: ref.Endpoint}
	values := map[string]string{}
	selectors := map[string]bool{"HOME": true, "XDG_CONFIG_HOME": true, "DOCKER_CONFIG": true, "CONTAINERS_CONF": true, "CONTAINERS_CONF_OVERRIDE": true, "CONTAINERS_CONF_MODULES": true, "PODMAN_CONNECTIONS_CONF": true, "XDG_RUNTIME_DIR": true, "DOCKER_HOST": true, "CONTAINER_HOST": true}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !selectors[key] {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return routingPlan{}, fmt.Errorf("duplicate routing environment selector %s", key)
		}
		values[key] = value
	}
	home := values["HOME"]
	if home == "" {
		account, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
		if err != nil {
			return routingPlan{}, errors.New("cannot determine authenticated caller home")
		}
		home = account.HomeDir
	}
	if err := routingAbsolute(home); err != nil {
		return routingPlan{}, fmt.Errorf("HOME: %w", err)
	}
	if values["CONTAINERS_CONF_MODULES"] != "" {
		return routingPlan{}, errors.New("CONTAINERS_CONF_MODULES is unsupported for contained launches")
	}
	configHomes := []string{filepath.Join(home, ".config")}
	if x := values["XDG_CONFIG_HOME"]; x != "" {
		if err := routingAbsolute(x); err != nil {
			return routingPlan{}, fmt.Errorf("XDG_CONFIG_HOME: %w", err)
		}
		configHomes = append(configHomes, x)
	}
	for i, dir := range systemDirs {
		if err := p.containerConfig(filepath.Join(dir, "containers.conf"), false, i == 0); err != nil {
			return routingPlan{}, err
		}
		if err := p.dropIns(filepath.Join(dir, "containers.conf.d")); err != nil {
			return routingPlan{}, err
		}
		if err := p.modules(filepath.Join(dir, "containers.conf.modules"), 0); err != nil {
			return routingPlan{}, err
		}
	}
	if uid != 0 && len(systemDirs) > 1 {
		rootless := filepath.Join(systemDirs[1], "containers.rootless.conf")
		if err := p.containerConfig(rootless, false, false); err != nil {
			return routingPlan{}, err
		}
		if err := p.dropIns(rootless + ".d"); err != nil {
			return routingPlan{}, err
		}
		if err := p.dropIns(filepath.Join(rootless+".d", strconv.FormatUint(uint64(uid), 10))); err != nil {
			return routingPlan{}, err
		}
	}
	for _, dir := range configHomes {
		base := filepath.Join(dir, "containers", "containers.conf")
		if err := p.containerConfig(base, false, false); err != nil {
			return routingPlan{}, err
		}
		if err := p.dropIns(base + ".d"); err != nil {
			return routingPlan{}, err
		}
		if err := p.modules(base+".modules", 0); err != nil {
			return routingPlan{}, err
		}
	}
	for _, key := range []string{"CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE"} {
		if path := values[key]; path != "" {
			if err := p.containerConfig(path, key == "CONTAINERS_CONF", true); err != nil {
				return routingPlan{}, fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	for _, dir := range configHomes {
		if err := p.connections(filepath.Join(dir, "containers", "podman-connections.json")); err != nil {
			return routingPlan{}, err
		}
	}
	if path := values["PODMAN_CONNECTIONS_CONF"]; path != "" {
		if err := p.connections(path); err != nil {
			return routingPlan{}, err
		}
	}
	dockerDirs := []string{filepath.Join(home, ".docker")}
	if path := values["DOCKER_CONFIG"]; path != "" {
		if err := routingAbsolute(path); err != nil {
			return routingPlan{}, fmt.Errorf("DOCKER_CONFIG: %w", err)
		}
		dockerDirs = append(dockerDirs, path)
	}
	for _, dir := range dockerDirs {
		if err := p.docker(dir); err != nil {
			return routingPlan{}, err
		}
	}
	for _, key := range []string{"DOCKER_HOST", "CONTAINER_HOST"} {
		if value := values[key]; value != "" {
			if err := p.destination(value); err != nil {
				return routingPlan{}, fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	for _, path := range []string{"/run/docker.sock", "/run/podman/podman.sock", fmt.Sprintf("/run/user/%d/podman/podman.sock", uid), fmt.Sprintf("/run/user/%d/docker.sock", uid)} {
		p.sockets[path] = true
	}
	if dir := values["XDG_RUNTIME_DIR"]; dir != "" {
		if err := routingAbsolute(dir); err != nil {
			return routingPlan{}, fmt.Errorf("XDG_RUNTIME_DIR: %w", err)
		}
		p.sockets[filepath.Join(dir, "podman", "podman.sock")] = true
		p.sockets[filepath.Join(dir, "docker.sock")] = true
	}
	for socket := range p.sockets {
		p.plan.Sockets = append(p.plan.Sockets, socket)
	}
	sort.Strings(p.plan.Sockets)
	sort.Slice(p.plan.Files, func(i, j int) bool { return p.plan.Files[i].Path < p.plan.Files[j].Path })
	p.plan.ReadOnlyDirs = sortedRoutingUnique(p.plan.ReadOnlyDirs)
	p.plan.EmptyDirs = sortedRoutingUnique(p.plan.EmptyDirs)
	return p.plan, nil
}

func sortedRoutingUnique(paths []string) []string {
	sort.Strings(paths)
	out := paths[:0]
	for _, p := range paths {
		if len(out) == 0 || out[len(out)-1] != p {
			out = append(out, p)
		}
	}
	return out
}
func routingAbsolute(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsRune(path, 0) {
		return errors.New("requires an absolute, clean, non-root path")
	}
	return nil
}

// openat2 resolves the entire pathname atomically without traversing symlinks.
// There is deliberately no weaker fallback on kernels lacking this primitive.
func openRoutingPath(path string, directory bool) (*os.File, error) {
	if err := routingAbsolute(path); err != nil {
		return nil, err
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{Flags: uint64(flags), Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("routing path has unsupported file type")
	}
	if !directory {
		var st unix.Stat_t
		if err = unix.Fstat(fd, &st); err != nil {
			f.Close()
			return nil, err
		}
		if st.Nlink != 1 {
			f.Close()
			return nil, errors.New("hard-linked routing files are unsupported")
		}
	}
	return f, nil
}
func (p *routingPlanner) read(path, kind string, required bool) ([]byte, os.FileMode, bool, error) {
	if existing, ok := p.files[path]; ok {
		if existing != kind {
			return nil, 0, false, fmt.Errorf("conflicting routing file formats at %s", path)
		}
		return nil, 0, true, nil
	}
	if len(p.files) >= maxRoutingEntries {
		return nil, 0, false, errors.New("too many routing configuration files")
	}
	p.files[path] = kind
	f, err := openRoutingPath(path, false)
	if errors.Is(err, os.ErrNotExist) && !required {
		mode := os.FileMode(0600)
		// Missing system configuration inherits a root-owned parent in the mount
		// helper. A synthesized, credential-free TOML default must remain readable
		// to the authenticated caller. Existing private files keep their modes.
		if kind == "toml" {
			mode = 0644
		}
		return nil, mode, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("read routing configuration %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRoutingFile+1))
	if err != nil {
		return nil, 0, false, err
	}
	p.total += len(data)
	if len(data) > maxRoutingFile || p.total > 32<<20 {
		return nil, 0, false, errors.New("routing configuration size limit exceeded")
	}
	return data, info.Mode().Perm(), false, nil
}
func routingEntries(path string) ([]os.DirEntry, error) {
	f, err := openRoutingPath(path, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read routing directory %s: %w", path, err)
	}
	defer f.Close()
	entries, err := f.ReadDir(maxRoutingEntries + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > maxRoutingEntries {
		return nil, errors.New("too many routing directory entries")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
func (p *routingPlanner) dropIns(path string) error {
	entries, err := routingEntries(path)
	if err != nil {
		return err
	}
	p.plan.ReadOnlyDirs = append(p.plan.ReadOnlyDirs, path)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".conf") {
			if err := p.containerConfig(filepath.Join(path, entry.Name()), true, false); err != nil {
				return err
			}
		}
	}
	return nil
}
func (p *routingPlanner) modules(path string, depth int) error {
	if depth > 8 {
		return errors.New("routing module nesting limit exceeded")
	}
	entries, err := routingEntries(path)
	if err != nil {
		return err
	}
	if depth == 0 {
		p.plan.ReadOnlyDirs = append(p.plan.ReadOnlyDirs, path)
	}
	for _, entry := range entries {
		name := filepath.Join(path, entry.Name())
		if entry.IsDir() {
			if err := p.modules(name, depth+1); err != nil {
				return err
			}
		} else {
			if err := p.containerConfig(name, true, false); err != nil {
				return err
			}
		}
	}
	return nil
}
func routingTable(parent map[string]any, key string) (map[string]any, error) {
	value, ok := parent[key]
	if !ok {
		value = map[string]any{}
		parent[key] = value
	}
	table, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("routing field %s must be a table", key)
	}
	return table, nil
}
func (p *routingPlanner) containerConfig(path string, required, defaultActive bool) error {
	data, mode, seen, err := p.read(path, "toml", required)
	if err != nil || seen {
		return err
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("invalid containers TOML at %s", path)
	}
	engine, err := routingTable(doc, "engine")
	if err != nil {
		return err
	}
	if value, ok := engine["remote"]; ok {
		if _, ok := value.(bool); !ok {
			return errors.New("containers engine.remote must be boolean")
		}
	}
	engine["remote"] = true
	for _, key := range []string{"remote_uri", "remote_identity"} {
		if value, ok := engine[key]; ok {
			text, valid := value.(string)
			if !valid {
				return fmt.Errorf("containers engine.%s must be a string", key)
			}
			if key == "remote_uri" {
				if err := p.destination(text); err != nil {
					return err
				}
			}
		}
	}
	engine["remote_uri"] = p.endpoint
	engine["remote_identity"] = ""
	active := ""
	if value, ok := engine["active_service"]; ok {
		var valid bool
		active, valid = value.(string)
		if !valid {
			return errors.New("containers active_service must be a string")
		}
	}
	if active != "" {
		p.active = active
	} else if defaultActive {
		engine["active_service"] = runtimeConnectionName
	}
	destinations, err := routingTable(engine, "service_destinations")
	if err != nil {
		return err
	}
	for name, value := range destinations {
		dst, ok := value.(map[string]any)
		if !ok {
			return errors.New("containers service destination must be a table")
		}
		for key := range dst {
			if key != "uri" && key != "identity" && key != "is_machine" {
				return fmt.Errorf("unsupported containers destination field %s", key)
			}
		}
		if value, ok := dst["uri"]; ok {
			uri, ok := value.(string)
			if !ok {
				return errors.New("containers destination uri must be a string")
			}
			if err := p.destination(uri); err != nil {
				return err
			}
		}
		if value, ok := dst["identity"]; ok {
			if _, ok := value.(string); !ok {
				return errors.New("containers destination identity must be a string")
			}
		}
		if value, ok := dst["is_machine"]; ok {
			if _, ok := value.(bool); !ok {
				return errors.New("containers destination is_machine must be boolean")
			}
		}
		dst["uri"] = p.endpoint
		dst["identity"] = ""
		dst["is_machine"] = false
		destinations[name] = dst
	}
	if _, ok := destinations[runtimeConnectionName]; !ok {
		destinations[runtimeConnectionName] = map[string]any{"uri": p.endpoint, "identity": "", "is_machine": false}
	}
	encoded, err := toml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode projected containers configuration %s: %w", path, err)
	}
	p.plan.Files = append(p.plan.Files, routingFile{Path: path, Data: encoded, Mode: mode})
	return nil
}

// JSON is decoded without float conversion or duplicate keys. Case aliases of
// routing fields are canonicalized separately, matching encoding/json clients.
func routingJSON(data []byte) (map[string]any, error) {
	if data == nil {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := routingJSONValue(dec, 0)
	if err != nil {
		return nil, errors.New("invalid routing JSON")
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, errors.New("trailing routing JSON")
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("routing JSON must be an object")
	}
	return result, nil
}
func routingJSONValue(dec *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("JSON nesting limit")
	}
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		result := map[string]any{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid key")
			}
			if _, ok := result[key]; ok {
				return nil, errors.New("duplicate key")
			}
			value, err := routingJSONValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
		_, err := dec.Token()
		return result, err
	case '[':
		result := []any{}
		for dec.More() {
			value, err := routingJSONValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		_, err := dec.Token()
		return result, err
	}
	return nil, errors.New("invalid delimiter")
}
func routingCanonical(table map[string]any, keys ...string) error {
	for _, canonical := range keys {
		found := ""
		for key := range table {
			if strings.EqualFold(key, canonical) {
				if found != "" {
					return fmt.Errorf("ambiguous routing JSON field %s", canonical)
				}
				found = key
			}
		}
		if found != "" && found != canonical {
			table[canonical] = table[found]
			delete(table, found)
		}
	}
	return nil
}
func (p *routingPlanner) jsonFile(path, kind string) (map[string]any, os.FileMode, bool, error) {
	data, mode, seen, err := p.read(path, kind, false)
	if err != nil || seen {
		return nil, mode, seen, err
	}
	if data == nil {
		// Synthesized metadata carries no credentials. Root-owned alternate
		// parents must still permit the caller to read the private view.
		mode = 0644
	}
	doc, err := routingJSON(data)
	if err != nil {
		return nil, mode, false, fmt.Errorf("%s: %w", path, err)
	}
	return doc, mode, false, nil
}
func (p *routingPlanner) addJSON(path string, doc map[string]any, mode os.FileMode) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	p.plan.Files = append(p.plan.Files, routingFile{Path: path, Data: append(data, '\n'), Mode: mode})
	return nil
}
func (p *routingPlanner) connections(path string) error {
	doc, mode, seen, err := p.jsonFile(path, "connections")
	if err != nil || seen {
		return err
	}
	if err := routingCanonical(doc, "Connection"); err != nil {
		return err
	}
	connection, err := routingTable(doc, "Connection")
	if err != nil {
		return err
	}
	if err := routingCanonical(connection, "Default", "Connections"); err != nil {
		return err
	}
	destinations, err := routingTable(connection, "Connections")
	if err != nil {
		return err
	}
	for name, value := range destinations {
		dst, ok := value.(map[string]any)
		if !ok {
			return errors.New("saved Podman connection must be an object")
		}
		if err := routingCanonical(dst, "URI", "Identity", "IsMachine"); err != nil {
			return err
		}
		for key := range dst {
			if key != "URI" && key != "Identity" && key != "IsMachine" {
				return fmt.Errorf("unsupported saved Podman connection field %s", key)
			}
		}
		if value, ok := dst["URI"]; ok {
			uri, ok := value.(string)
			if !ok {
				return errors.New("saved connection URI must be a string")
			}
			if err := p.destination(uri); err != nil {
				return err
			}
		}
		if value, ok := dst["Identity"]; ok {
			if _, ok := value.(string); !ok {
				return errors.New("saved connection identity must be a string")
			}
		}
		if value, ok := dst["IsMachine"]; ok {
			if _, ok := value.(bool); !ok {
				return errors.New("saved connection IsMachine must be boolean")
			}
		}
		dst["URI"] = p.endpoint
		dst["Identity"] = ""
		dst["IsMachine"] = false
		destinations[name] = dst
	}
	def := ""
	if value, ok := connection["Default"]; ok {
		var valid bool
		def, valid = value.(string)
		if !valid {
			return errors.New("saved connection Default must be a string")
		}
	}
	if def == "" {
		if p.active != "" {
			def = p.active
		} else {
			def = runtimeConnectionName
		}
		connection["Default"] = def
	}
	if _, ok := destinations[def]; !ok {
		destinations[def] = map[string]any{"URI": p.endpoint, "Identity": "", "IsMachine": false}
	}
	return p.addJSON(path, doc, mode)
}
func (p *routingPlanner) docker(dir string) error {
	path := filepath.Join(dir, "config.json")
	doc, mode, seen, err := p.jsonFile(path, "docker")
	if err != nil || seen {
		return err
	}
	if err := routingCanonical(doc, "currentContext"); err != nil {
		return err
	}
	if value, ok := doc["currentContext"]; ok {
		if _, ok := value.(string); !ok {
			return errors.New("Docker currentContext must be a string")
		}
	}
	// Keep the selected context name; all stored Docker endpoints are projected.
	if err := p.addJSON(path, doc, mode); err != nil {
		return err
	}
	meta := filepath.Join(dir, "contexts", "meta")
	entries, err := routingEntries(meta)
	if err != nil {
		return err
	}
	p.plan.ReadOnlyDirs = append(p.plan.ReadOnlyDirs, meta)
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 64 || strings.Trim(entry.Name(), "0123456789abcdef") != "" {
			return errors.New("unsupported Docker context metadata layout")
		}
		path := filepath.Join(meta, entry.Name(), "meta.json")
		data, mode, seen, err := p.read(path, "docker-context", true)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		context, err := routingJSON(data)
		if err != nil {
			return fmt.Errorf("invalid Docker context metadata %s", path)
		}
		if err := routingCanonical(context, "Name", "Endpoints"); err != nil {
			return err
		}
		name, ok := context["Name"].(string)
		if !ok || name == "" || name == "default" {
			return errors.New("invalid stored Docker context name")
		}
		if fmt.Sprintf("%x", sha256.Sum256([]byte(name))) != entry.Name() {
			return errors.New("Docker context name does not match metadata directory")
		}
		endpoints, err := routingTable(context, "Endpoints")
		if err != nil {
			return err
		}
		endpoint, err := routingTable(endpoints, "docker")
		if err != nil {
			return err
		}
		if err := routingCanonical(endpoint, "Host", "SkipTLSVerify"); err != nil {
			return err
		}
		for key := range endpoint {
			if key != "Host" && key != "SkipTLSVerify" {
				return fmt.Errorf("unsupported Docker endpoint field %s", key)
			}
		}
		host, ok := endpoint["Host"].(string)
		if !ok {
			return errors.New("Docker context Host must be a string")
		}
		if err := p.destination(host); err != nil {
			return err
		}
		if value, ok := endpoint["SkipTLSVerify"]; ok {
			if _, ok := value.(bool); !ok {
				return errors.New("Docker context SkipTLSVerify must be boolean")
			}
		}
		endpoint["Host"] = p.endpoint
		endpoint["SkipTLSVerify"] = false
		tlsDir := filepath.Join(dir, "contexts", "tls", entry.Name(), "docker")
		tlsHandle, err := openRoutingPath(tlsDir, true)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unsafe Docker TLS directory: %w", err)
		}
		if tlsHandle != nil {
			tlsHandle.Close()
		}
		p.plan.EmptyDirs = append(p.plan.EmptyDirs, tlsDir)
		if err := p.addJSON(path, context, mode); err != nil {
			return err
		}
	}
	return nil
}
func (p *routingPlanner) destination(value string) error {
	if value == "" {
		return nil
	}
	uri, err := url.Parse(value)
	if err != nil || uri.Scheme == "" {
		return errors.New("malformed container API destination")
	}
	switch uri.Scheme {
	case "unix":
		if uri.Host != "" || uri.User != nil || uri.RawQuery != "" || uri.ForceQuery || uri.RawPath != "" || uri.Fragment != "" || uri.Opaque != "" {
			return errors.New("unsupported Unix container API destination")
		}
		if err := routingAbsolute(uri.Path); err != nil {
			return fmt.Errorf("Unix container API destination: %w", err)
		}
		p.sockets[uri.Path] = true
	case "tcp", "ssh", "http", "https":
		if uri.Host == "" {
			return errors.New("container API destination requires a host")
		}
	default:
		return fmt.Errorf("unsupported container API destination scheme %s", uri.Scheme)
	}
	return nil
}
