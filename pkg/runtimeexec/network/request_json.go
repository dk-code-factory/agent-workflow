package network

import (
	"encoding/base64"
	"encoding/json"
	"errors"
)

const requestByteEncoding = "base64-v1"

var errRequestEncoding = errors.New("invalid network launch byte encoding")

type requestFields Request // No JSON methods: metadata keeps its existing representation.

// MarshalJSON preserves native strings as bytes, including invalid UTF-8. A
// value receiver covers Request values nested inside launch metadata as well.
func (r Request) MarshalJSON() ([]byte, error) {
	fields := requestFields(r)
	if r.Path == "" && len(r.Args) == 0 && len(r.Env) == 0 && r.Dir == "" {
		return json.Marshal(fields)
	}
	wire := struct {
		requestFields
		Encoding  string   `json:"byte_encoding"`
		PathBytes []byte   `json:"path_bytes,omitempty"`
		ArgsBytes [][]byte `json:"args_bytes,omitempty"`
		EnvBytes  [][]byte `json:"env_bytes,omitempty"`
		DirBytes  []byte   `json:"dir_bytes,omitempty"`
	}{requestFields: fields, Encoding: requestByteEncoding, PathBytes: []byte(r.Path), DirBytes: []byte(r.Dir)}
	wire.Path, wire.Args, wire.Env, wire.Dir = "", nil, nil, ""
	for _, arg := range r.Args {
		wire.ArgsBytes = append(wire.ArgsBytes, []byte(arg))
	}
	for _, env := range r.Env {
		wire.EnvBytes = append(wire.EnvBytes, []byte(env))
	}
	return json.Marshal(wire)
}

func (r *Request) UnmarshalJSON(data []byte) error {
	// RawMessage tracks absent versus explicitly null/empty fields while retaining
	// the standard library's duplicate-key and case-insensitive field behavior.
	var wire struct {
		requestFields
		Encoding  json.RawMessage `json:"byte_encoding"`
		PathBytes json.RawMessage `json:"path_bytes"`
		ArgsBytes json.RawMessage `json:"args_bytes"`
		EnvBytes  json.RawMessage `json:"env_bytes"`
		DirBytes  json.RawMessage `json:"dir_bytes"`
	}
	if json.Unmarshal(data, &wire) != nil {
		return errRequestEncoding
	}
	if len(wire.Encoding) == 0 {
		if len(wire.PathBytes)+len(wire.ArgsBytes)+len(wire.EnvBytes)+len(wire.DirBytes) != 0 {
			return errRequestEncoding
		}
		*r = Request(wire.requestFields) // Existing ordinary UTF-8 callers remain supported.
		return nil
	}
	var encoding string
	if json.Unmarshal(wire.Encoding, &encoding) != nil || encoding != requestByteEncoding {
		return errRequestEncoding
	}
	if wire.Path != "" || len(wire.Args) != 0 || len(wire.Env) != 0 || wire.Dir != "" {
		return errRequestEncoding
	}
	var err error
	if wire.Path, err = decodeRequestBytes(wire.PathBytes); err != nil {
		return err
	}
	if wire.Args, err = decodeRequestByteList(wire.ArgsBytes); err != nil {
		return err
	}
	if wire.Env, err = decodeRequestByteList(wire.EnvBytes); err != nil {
		return err
	}
	if wire.Dir, err = decodeRequestBytes(wire.DirBytes); err != nil {
		return err
	}
	*r = Request(wire.requestFields)
	return nil
}

func decodeRequestBytes(data json.RawMessage) (string, error) {
	if len(data) == 0 || string(data) == "null" {
		return "", nil
	}
	// []byte's standard JSON decoder also accepts number arrays. Require the
	// string/null base64 shape explicitly, without reporting secret field values.
	if data[0] != '"' {
		return "", errRequestEncoding
	}
	var encoded string
	if json.Unmarshal(data, &encoded) != nil {
		return "", errRequestEncoding
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", errRequestEncoding
	}
	return string(decoded), nil
}

func decodeRequestByteList(data json.RawMessage) ([]string, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	if data[0] != '[' {
		return nil, errRequestEncoding
	}
	var items []json.RawMessage
	if json.Unmarshal(data, &items) != nil {
		return nil, errRequestEncoding
	}
	out := make([]string, len(items))
	for i, item := range items {
		value, err := decodeRequestBytes(item)
		if err != nil {
			return nil, err
		}
		out[i] = value
	}
	return out, nil
}
