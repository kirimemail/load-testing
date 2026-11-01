package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kirimemail/load-testing/internal/common"
)

func testConfig() common.TestConfig {
	return common.TestConfig{
		SMTPHost: "default.relay", SMTPPort: 587,
		SMTPUser: "u", SMTPPass: "p",
		FromDomain:             "loadtest.example",
		Recipients:             []string{"sink@loadtest.example"},
		TargetRatePerGenerator: 10, DurationSeconds: 5,
		PayloadBytes: 1024, Profile: common.ProfilePersistent,
		Connections: 2, TLSVerify: false, MaxDurationSecs: 10,
	}
}

// Each generator must receive its own relay host (gen-i -> relay-i).
func TestStartTestRelayMapping(t *testing.T) {
	received := make([]common.TestConfig, 2)
	var servers []*httptest.Server
	for i := range received {
		i := i
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/configure" {
				var cfg common.TestConfig
				if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				received[i] = cfg
			}
			w.Write([]byte(`{}`))
		}))
		defer s.Close()
		servers = append(servers, s)
	}
	ctl := New([]string{servers[0].URL, servers[1].URL})
	if err := ctl.StartTest("test", testConfig(), []string{"relay-a", "relay-b"}); err != nil {
		t.Fatalf("StartTest: %v", err)
	}
	if received[0].SMTPHost != "relay-a" || received[1].SMTPHost != "relay-b" {
		t.Fatalf("wrong per-generator mapping: %q %q",
			received[0].SMTPHost, received[1].SMTPHost)
	}
}

// A RELAY_HOSTS list that does not match the fleet must fail loudly.
func TestStartTestRelayLengthMismatch(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer s.Close()
	ctl := New([]string{s.URL, s.URL})
	if err := ctl.StartTest("test", testConfig(), []string{"only-one"}); err == nil {
		t.Fatal("expected length-mismatch error, got nil")
	}
}
