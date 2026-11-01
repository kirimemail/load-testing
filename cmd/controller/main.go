package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/kirimemail/load-testing/internal/common"
	"github.com/kirimemail/load-testing/internal/controller"
)

func main() {
	gens := splitCSV(getenv("GENERATORS", "http://gen-1:8080,http://gen-2:8080,http://gen-3:8080,http://gen-4:8080"))
	ctl := controller.New(gens)
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok"}`)) })

	// POST /api/tests/start {"name":"scenario-b","config":{...}}
	mux.HandleFunc("/api/tests/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name   string             `json:"name"`
			Config common.TestConfig `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			req.Name = "adhoc"
		}
		if err := ctl.StartTest(req.Name, req.Config); err != nil {
			// 502 with per-generator detail so failures are never hidden.
			http.Error(w, "start failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"status": "started", "scenario": req.Name})
	})

	mux.HandleFunc("/api/tests/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"results": ctl.StopTest()})
	})

	// Emergency stop: aborts all generators immediately.
	mux.HandleFunc("/api/tests/emergency-stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"results": ctl.EmergencyStop()})
	})

	mux.HandleFunc("/api/tests/status", func(w http.ResponseWriter, r *http.Request) {
		statuses, errs := ctl.Collect()
		writeJSON(w, map[string]any{"generators": statuses, "errors": errs})
	})

	// Benchmark report: aggregate accepted vs attempted + p50/p95/p99.
	mux.HandleFunc("/api/report", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, ctl.Report())
	})

	addr := getenv("MANAGEMENT_ADDR", ":8081")
	log.Printf("controller listening on %s with %d generators", addr, len(gens))
	log.Fatal(http.ListenAndServe(addr, mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
