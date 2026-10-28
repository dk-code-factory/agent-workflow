package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func (s *Server) inputAPI(w http.ResponseWriter, r *http.Request, run *workflows.Run, parts []string) bool {
	if len(parts) < 3 || (parts[2] != "inputs" && parts[2] != "interrupt") {
		return false
	}
	fail := func(status int, err error) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
	}
	encode := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if parts[2] == "inputs" && len(parts) == 3 && r.Method == "GET" {
		gates, err := s.Store.Inputs(r.Context(), run.ID)
		if err != nil {
			fail(500, err)
		} else {
			encode(gates)
		}
		return true
	}
	if run.OriginID != "" && run.OriginID != run.ID {
		fail(409, fmt.Errorf("control the originating run %s", run.OriginID))
		return true
	}
	if parts[2] == "inputs" && len(parts) == 5 && parts[4] == "answer" && r.Method == "POST" {
		var a workflows.ExternalInputAnswer
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&a); err != nil {
			fail(400, err)
			return true
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			fail(400, fmt.Errorf("expected one JSON answer"))
			return true
		}
		g, err := s.Store.AcceptInput(r.Context(), run.ID, parts[3], a)
		if err != nil {
			fail(409, err)
			return true
		}
		select {
		case s.wake <- struct{}{}:
		default:
		}
		w.WriteHeader(202)
		encode(g)
		return true
	}
	if parts[2] == "interrupt" && len(parts) == 3 && r.Method == "POST" {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.Store.InterruptInput(r.Context(), run.ID, run.Generation); err != nil {
			fail(409, err)
			return true
		}
		if cancel := s.active[run.ID]; cancel != nil {
			cancel()
		}
		encode(map[string]any{"id": run.ID, "interruption_requested": true})
		return true
	}
	fail(405, fmt.Errorf("unsupported input operation"))
	return true
}
