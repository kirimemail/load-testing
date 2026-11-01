// Package common holds the shared test-configuration contract between the
// controller and the generators. JSON is the wire format so both Go binaries
// and the YAML scenario files map onto the same struct.
package common

import "fmt"

// Connection profiles supported by the generator.
const (
	ProfilePersistent = "persistent"
	ProfileChurn      = "churn"
)

// TestConfig is the unit of work assigned to ONE generator.
// Aggregate targets are obtained by multiplying by the generator count.
type TestConfig struct {
	// SMTP target (the Haraka relay paired with this generator).
	SMTPHost string `json:"smtp_host" yaml:"smtp_host"`
	SMTPPort int    `json:"smtp_port" yaml:"smtp_port"`

	// SMTP AUTH credentials. Never logged.
	SMTPUser string `json:"smtp_user" yaml:"smtp_user"`
	SMTPPass string `json:"smtp_pass" yaml:"smtp_pass"`

	// Envelope shaping. All traffic MUST stay inside the dedicated test domain.
	FromDomain string   `json:"from_domain" yaml:"from_domain"`
	Recipients []string `json:"recipients" yaml:"recipients"`

	// Load shaping.
	TargetRatePerGenerator int    `json:"target_rate_per_generator" yaml:"target_rate_per_generator"` // msg/sec
	DurationSeconds        int    `json:"duration_seconds" yaml:"duration_seconds"`
	PayloadBytes           int    `json:"payload_bytes" yaml:"payload_bytes"`
	Profile                string `json:"profile" yaml:"profile"` // persistent | churn
	Connections            int    `json:"connections" yaml:"connections"`

	// Safety / transport.
	TLSVerify       bool `json:"tls_verify" yaml:"tls_verify"` // default true, never disable in prod
	MaxDurationSecs int  `json:"max_duration_secs" yaml:"max_duration_secs"`

	// Generator identity (informational, set by controller).
	GeneratorID string `json:"generator_id,omitempty" yaml:"generator_id,omitempty"`
}

// Scenario is a named TestConfig template expanded per generator by the controller.
type Scenario struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	TestConfig  `yaml:",inline"`
	Generators  int      `yaml:"generators"`
	RelayHosts  []string `yaml:"relay_hosts,omitempty"`
}

// Validate enforces safety rules before any load is emitted.
func (c *TestConfig) Validate() error {
	if c.SMTPHost == "" {
		return fmt.Errorf("smtp_host is required")
	}
	if c.SMTPPort <= 0 || c.SMTPPort > 65535 {
		return fmt.Errorf("smtp_port out of range")
	}
	if c.TargetRatePerGenerator <= 0 {
		return fmt.Errorf("target_rate_per_generator must be > 0")
	}
	if c.DurationSeconds <= 0 {
		return fmt.Errorf("duration_seconds must be > 0")
	}
	if c.MaxDurationSecs > 0 && c.DurationSeconds > c.MaxDurationSecs {
		return fmt.Errorf("duration %ds exceeds max_duration %ds", c.DurationSeconds, c.MaxDurationSecs)
	}
	if c.PayloadBytes < 512 || c.PayloadBytes > 1<<20 {
		return fmt.Errorf("payload_bytes must be within [512, 1MiB]")
	}
	if c.Profile != ProfilePersistent && c.Profile != ProfileChurn {
		return fmt.Errorf("profile must be %q or %q", ProfilePersistent, ProfileChurn)
	}
	if c.Connections <= 0 || c.Connections > 1000 {
		return fmt.Errorf("connections must be within [1, 1000]")
	}
	if len(c.Recipients) == 0 {
		return fmt.Errorf("at least one sink recipient is required")
	}
	if c.FromDomain == "" {
		return fmt.Errorf("from_domain is required (dedicated test domain)")
	}
	return nil
}

// GeneratorStatus is returned by GET /status on a generator.
type GeneratorStatus struct {
	GeneratorID string  `json:"generator_id"`
	State       string  `json:"state"` // idle|running|stopping|stopped|emergency
	Attempted   uint64  `json:"attempted"`
	Accepted    uint64  `json:"accepted"`
	Failed      uint64  `json:"failed"`
	Dropped     uint64  `json:"dropped"`
	ConnActive  int     `json:"connections_active"`
	ConnTotal   uint64  `json:"connections_total"`
	TLSTotal    uint64  `json:"tls_handshakes_total"`
	AuthFails   uint64  `json:"auth_failures_total"`
	RateActual  float64 `json:"actual_rate_msg_sec"`
	P50Ms       float64 `json:"latency_p50_ms"`
	P95Ms       float64 `json:"latency_p95_ms"`
	P99Ms       float64 `json:"latency_p99_ms"`
	ElapsedSecs float64 `json:"elapsed_seconds"`
}

// AggregateReport is returned by the controller: end-to-end benchmark summary.
type AggregateReport struct {
	Scenario          string            `json:"scenario"`
	Generators        int               `json:"generators"`
	TargetTotalRate   int               `json:"target_total_msg_sec"`
	Attempted         uint64            `json:"attempted_total"`
	Accepted          uint64            `json:"accepted_total"`
	Failed            uint64            `json:"failed_total"`
	Dropped           uint64            `json:"dropped_total"`
	AcceptedRate      float64           `json:"accepted_msg_sec"`
	AttemptedRate     float64           `json:"attempted_msg_sec"`
	LatencyP50Ms      float64           `json:"latency_p50_ms"`
	LatencyP95Ms      float64           `json:"latency_p95_ms"`
	LatencyP99Ms      float64           `json:"latency_p99_ms"`
	ElapsedSecs       float64           `json:"elapsed_seconds"`
	ExpectedMessages  uint64            `json:"expected_messages"`
	NetworkNote       string            `json:"network_note"`
	PerGenerator      []GeneratorStatus `json:"per_generator"`
}
