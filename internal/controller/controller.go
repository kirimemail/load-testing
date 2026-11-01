// Package controller orchestrates generator fleets. It fans test configs out
// to generators, scrapes their /status endpoints for aggregation, and renders
// benchmark reports. It is deliberately NOT in the SMTP data path, so it can
// never become a throughput bottleneck: workers execute independently and the
// controller only exchanges small JSON control messages.
package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/kirimemail/load-testing/internal/common"
)

// Controller holds fleet membership and the active test definition.
type Controller struct {
	mu         sync.Mutex
	generators []string // generator base URLs, e.g. http://gen-1:8080
	active     *ActiveTest
	client     *http.Client
}

type ActiveTest struct {
	Name      string             `json:"name"`
	Config    common.TestConfig  `json:"config"`
	StartedAt time.Time          `json:"started_at"`
}

func New(generators []string) *Controller {
	return &Controller{generators: generators, client: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Controller) SetGenerators(urls []string) { c.mu.Lock(); c.generators = urls; c.mu.Unlock() }
func (c *Controller) Generators() []string        { c.mu.Lock(); defer c.mu.Unlock(); return append([]string(nil), c.generators...) }

// StartTest validates once, then configures + starts every generator in
// parallel. A per-generator failure is reported, never hidden.
func (c *Controller) StartTest(name string, cfg common.TestConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	gens := c.Generators()
	if len(gens) == 0 {
		return fmt.Errorf("no generators registered")
	}
	type res struct {
		gen string
		err error
	}
	results := make(chan res, len(gens))
	var wg sync.WaitGroup
	for _, g := range gens {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			if err := c.postJSON(base+"/configure", cfg); err != nil {
				results <- res{base, fmt.Errorf("configure: %w", err)}
				return
			}
			if err := c.postJSON(base+"/start", map[string]any{}); err != nil {
				results <- res{base, fmt.Errorf("start: %w", err)}
				return
			}
			results <- res{base, nil}
		}(g)
	}
	wg.Wait()
	close(results)
	var failed []string
	for r := range results {
		if r.err != nil {
			failed = append(failed, r.gen+": "+r.err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d/%d generators failed: %v", len(failed), len(gens), failed)
	}
	c.mu.Lock()
	c.active = &ActiveTest{Name: name, Config: cfg, StartedAt: time.Now()}
	c.mu.Unlock()
	return nil
}

// StopTest fans out graceful stop; EmergencyStop aborts immediately.
func (c *Controller) StopTest() map[string]string     { return c.fanout("/stop") }
func (c *Controller) EmergencyStop() map[string]string { return c.fanout("/emergency-stop") }

func (c *Controller) fanout(path string) map[string]string {
	gens := c.Generators()
	type res struct {
		gen string
		msg string
	}
	results := make(chan res, len(gens))
	var wg sync.WaitGroup
	for _, g := range gens {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			msg := "ok"
			if err := c.postJSON(base+path, map[string]any{}); err != nil {
				msg = err.Error()
			}
			results <- res{base, msg}
		}(g)
	}
	wg.Wait()
	close(results)
	out := make(map[string]string, len(gens))
	for r := range results {
		out[r.gen] = r.msg
	}
	return out
}

// Collect scrapes every generator /status concurrently.
func (c *Controller) Collect() ([]common.GeneratorStatus, []string) {
	gens := c.Generators()
	statuses := make([]common.GeneratorStatus, 0, len(gens))
	var errs []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, g := range gens {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			var st common.GeneratorStatus
			if err := c.getJSON(base+"/status", &st); err != nil {
				mu.Lock()
				errs = append(errs, base+": "+err.Error())
				mu.Unlock()
				return
			}
			mu.Lock()
			statuses = append(statuses, st)
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].GeneratorID < statuses[j].GeneratorID })
	return statuses, errs
}

// Report aggregates throughput + latency. Target is ACCEPTED msg/sec;
// attempted is reported independently so inflated numbers are impossible.
func (c *Controller) Report() common.AggregateReport {
	statuses, _ := c.Collect()
	c.mu.Lock()
	name := ""
	cfg := common.TestConfig{}
	var started time.Time
	if c.active != nil {
		name = c.active.Name
		cfg = c.active.Config
		started = c.active.StartedAt
	}
	n := len(c.generators)
	c.mu.Unlock()

	var attempted, accepted, failed, dropped uint64
	// Weighted latency merge by accepted count (approximation; exact merge
	// would require shipping full reservoirs — documented limitation).
	var wP50, wP95, wP99 float64
	var elapsed float64
	if !started.IsZero() {
		elapsed = time.Since(started).Seconds()
	}
	for _, s := range statuses {
		attempted += s.Attempted
		accepted += s.Accepted
		failed += s.Failed
		dropped += s.Dropped
		if s.ElapsedSecs > elapsed {
			elapsed = s.ElapsedSecs
		}
		if accepted > 0 {
			w := float64(s.Accepted)
			wP50 += s.P50Ms * w
			wP95 += s.P95Ms * w
			wP99 += s.P99Ms * w
		}
	}
	var p50, p95, p99 float64
	if accepted > 0 {
		p50, p95, p99 = wP50/float64(accepted), wP95/float64(accepted), wP99/float64(accepted)
	}
	var accRate, attRate float64
	if elapsed > 0 {
		accRate = float64(accepted) / elapsed
		attRate = float64(attempted) / elapsed
	}
	var expected uint64
	targetTotal := 0
	if cfg.TargetRatePerGenerator > 0 {
		targetTotal = cfg.TargetRatePerGenerator * n
		expected = uint64(targetTotal * cfg.DurationSeconds)
	}
	return common.AggregateReport{
		Scenario: name, Generators: n,
		TargetTotalRate: targetTotal,
		Attempted:       attempted, Accepted: accepted, Failed: failed, Dropped: dropped,
		AcceptedRate:  accRate,
		AttemptedRate: attRate,
		LatencyP50Ms:  p50, LatencyP95Ms: p95, LatencyP99Ms: p99,
		ElapsedSecs:     elapsed,
		ExpectedMessages: expected,
		NetworkNote: "Compare accepted_msg_sec against relay CPU/net + generator interface utilization to distinguish network saturation from Haraka saturation. See docs/network-triage.md.",
		PerGenerator: statuses,
	}
}

func (c *Controller) postJSON(url string, v any) error {
	buf, _ := json.Marshal(v)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *Controller) getJSON(url string, v any) error {
	resp, err := c.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
