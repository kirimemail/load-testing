package generator

import (
	"encoding/json"
	"net/http"

	"github.com/kirimemail/load-testing/internal/common"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server exposes the generator management API. Management traffic stays on
// this port, separate from SMTP test traffic. Never expose it publicly.
type Server struct {
	eng *Engine
	mux *http.ServeMux
}

func NewServer(id string) *Server {
	s := &Server{eng: NewEngine(id), mux: http.NewServeMux()}
	s.mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	s.mux.HandleFunc("/configure", s.handleConfigure)
	s.mux.HandleFunc("/start", s.handleStart)
	s.mux.HandleFunc("/stop", s.handleStop)
	s.mux.HandleFunc("/emergency-stop", s.handleEmergency)
	s.mux.HandleFunc("/status", s.handleStatus)
	s.mux.Handle("/metrics", promhttp.HandlerFor(s.eng.Registry(), promhttp.HandlerOpts{}))
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) handleConfigure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var cfg common.TestConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "bad config: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.eng.Configure(cfg); err != nil {
		http.Error(w, "invalid config: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"configured"}`))
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.eng.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Write([]byte(`{"status":"started"}`))
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.eng.Stop()
	w.Write([]byte(`{"status":"stopping"}`))
}

func (s *Server) handleEmergency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.eng.Panic()
	w.Write([]byte(`{"status":"emergency-stop"}`))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.eng.Status())
}
