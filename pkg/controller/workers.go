package controller

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

type SendRequest struct {
	RequestID string `json:"request_id"`
	Prompt    string `json:"prompt"`
}

func (s *Server) workerAPI(w http.ResponseWriter, r *http.Request, run string, path []string, secrets map[string]string) {
	fail := func(code int, err error) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": workflows.Redact(err.Error(), secrets)})
	}
	encode := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	decode := func(v any) error {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		d.DisallowUnknownFields()
		return d.Decode(v)
	}
	if len(path) == 0 {
		if r.Method == "GET" {
			out, err := s.Workers.List(r.Context(), run)
			if err != nil {
				fail(500, err)
				return
			}
			encode(out)
			return
		}
		if r.Method == "POST" {
			var spec workflows.WorkerSpec
			if err := decode(&spec); err != nil {
				fail(400, err)
				return
			}
			if spec.RunID != "" && spec.RunID != run {
				fail(403, fmt.Errorf("cross-run worker spawn denied"))
				return
			}
			spec.RunID = run
			out, err := s.Workers.Spawn(r.Context(), spec)
			if err != nil {
				fail(400, err)
				return
			}
			w.WriteHeader(202)
			encode(out)
			return
		}
	}
	if len(path) == 1 && r.Method == "GET" {
		out, err := s.Workers.Get(r.Context(), run, path[0])
		if err != nil {
			fail(404, err)
			return
		}
		encode(out)
		return
	}
	if len(path) == 2 {
		id := path[0]
		if path[1] == "messages" && r.Method == "GET" {
			out, err := s.Workers.Messages(r.Context(), run, id)
			if err != nil {
				fail(404, err)
				return
			}
			encode(out)
			return
		}
		if path[1] == "send" && r.Method == "POST" {
			var input SendRequest
			if err := decode(&input); err != nil {
				fail(400, err)
				return
			}
			out, err := s.Workers.Send(r.Context(), run, id, input.RequestID, input.Prompt)
			if err != nil {
				fail(400, err)
				return
			}
			w.WriteHeader(202)
			encode(out)
			return
		}
		if (path[1] == "interrupt" || path[1] == "close") && r.Method == "POST" {
			if err := s.Workers.Interrupt(r.Context(), run, id, path[1] == "close"); err != nil {
				fail(400, err)
				return
			}
			encode(map[string]string{"id": id, "operation": path[1]})
			return
		}
		if path[1] == "resume" && r.Method == "POST" {
			if r.URL.Query().Get("acknowledge") != "true" {
				fail(400, fmt.Errorf("worker recovery requires explicit acknowledge=true"))
				return
			}
			if err := s.Workers.Resume(r.Context(), run, id); err != nil {
				fail(400, err)
				return
			}
			encode(map[string]string{"id": id})
			return
		}
	}
	fail(405, fmt.Errorf("unsupported worker operation"))
}
