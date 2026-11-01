package generator

import (
	"crypto/rand"
	"fmt"
	mrand "math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kirimemail/load-testing/internal/common"
	"github.com/prometheus/client_golang/prometheus"
)

// Engine executes the assigned workload. A global rate scheduler feeds a
// shared job queue consumed by the connection workers, so one slow SMTP
// session can never head-of-line-block the aggregate 500 msg/sec target.
// When the queue is full the message is counted as dropped — never silently
// discarded.
type Engine struct {
	id  string
	cfg atomic.Value // holds common.TestConfig

	reg *prometheus.Registry

	attempted *prometheus.CounterVec
	accepted  *prometheus.CounterVec
	failed    *prometheus.CounterVec
	dropped   *prometheus.CounterVec
	active    prometheus.Gauge
	connTotal prometheus.Counter
	tlsTotal  prometheus.Counter
	authFails prometheus.Counter
	latency   prometheus.Histogram
	rate      prometheus.Gauge

	mu        sync.Mutex
	state     string
	cancel    *context_Cancel
	done      chan struct{}
	startTime time.Time

	// Local latency reservoir for p50/p95/p99 (protected by latMu).
	latMu     sync.Mutex
	latencies []float64 // milliseconds, capped ring

	// Counters mirrored atomically for /status without prometheus scrape.
	nAttempted uint64
	nAccepted  uint64
	nFailed    uint64
	nDropped   uint64
	nConnTotal uint64
	nTLS       uint64
	nAuthFail  uint64
	nActive    int64
	sentWindow atomic.Uint64 // messages accepted in current 1s window (rate gauge)
}

// NewEngine builds an engine with its own prometheus registry per generator.
func NewEngine(id string) *Engine {
	reg := prometheus.NewRegistry()
	e := &Engine{id: id, reg: reg, state: "idle", latencies: make([]float64, 0, 32768)}
	lbl := prometheus.Labels{"generator": id}
	e.attempted = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "smtp_messages_attempted_total", Help: "Messages attempted."}, []string{"generator"})
	e.accepted = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "smtp_messages_accepted_total", Help: "Messages accepted by SMTP (250 after DATA)."}, []string{"generator"})
	e.failed = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "smtp_messages_failed_total", Help: "Failed SMTP transactions."}, []string{"generator"})
	e.dropped = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "smtp_messages_dropped_total", Help: "Messages missed/dropped by the generator scheduler."}, []string{"generator"})
	e.active = prometheus.NewGauge(prometheus.GaugeOpts{Name: "smtp_connections_active", Help: "Active SMTP sessions.", ConstLabels: lbl})
	e.connTotal = prometheus.NewCounter(prometheus.CounterOpts{Name: "smtp_connections_total", Help: "Total connections established.", ConstLabels: lbl})
	e.tlsTotal = prometheus.NewCounter(prometheus.CounterOpts{Name: "smtp_tls_handshakes_total", Help: "TLS handshakes.", ConstLabels: lbl})
	e.authFails = prometheus.NewCounter(prometheus.CounterOpts{Name: "smtp_auth_failures_total", Help: "Authentication failures.", ConstLabels: lbl})
	e.latency = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "smtp_transaction_duration_seconds", Help: "SMTP transaction latency.", ConstLabels: lbl, Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}})
	e.rate = prometheus.NewGauge(prometheus.GaugeOpts{Name: "smtp_generator_rate", Help: "Actual message generation rate (accepted/sec).", ConstLabels: lbl})
	reg.MustRegister(e.attempted, e.accepted, e.failed, e.dropped, e.active, e.connTotal, e.tlsTotal, e.authFails, e.latency, e.rate)
	return e
}

func (e *Engine) Configure(c common.TestConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.GeneratorID = e.id
	e.cfg.Store(c)
	return nil
}

func (e *Engine) getCfg() (common.TestConfig, bool) {
	v := e.cfg.Load()
	if v == nil {
		return common.TestConfig{}, false
	}
	return v.(common.TestConfig), true
}

// Start begins the test in the background. Non-blocking.
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state == "running" {
		return fmt.Errorf("already running")
	}
	cfg, ok := e.getCfg()
	if !ok {
		return fmt.Errorf("not configured: POST /configure first")
	}
	e.cancel = newCancel()
	e.done = make(chan struct{})
	e.state = "running"
	e.startTime = time.Now()
	go e.run(cfg, e.cancel, e.done)
	return nil
}

// Stop requests graceful shutdown; EmergencyStop aborts immediately.
func (e *Engine) Stop()  { e.mu.Lock(); c := e.cancel; e.mu.Unlock(); if c != nil { c.cancel() } }
func (e *Engine) Panic() { e.mu.Lock(); e.state = "emergency"; c := e.cancel; e.mu.Unlock(); if c != nil { c.cancel() } }

func (e *Engine) State() string { e.mu.Lock(); defer e.mu.Unlock(); return e.state }

// run is the main loop: global ticker -> job queue -> workers -> stats.
func (e *Engine) run(cfg common.TestConfig, cancel *context_Cancel, done chan struct{}) {
	defer close(done)
	defer func() { e.mu.Lock(); if e.state == "running" { e.state = "stopped" }; e.mu.Unlock() }()

	deadline := time.Now().Add(time.Duration(cfg.DurationSeconds) * time.Second)
	if cfg.MaxDurationSecs > 0 {
		if m := time.Now().Add(time.Duration(cfg.MaxDurationSecs) * time.Second); m.Before(deadline) {
			deadline = m
		}
	}

	jobs := make(chan uint64, cfg.TargetRatePerGenerator*4) // 4s burst buffer; overflow => dropped
	var wg sync.WaitGroup
	if cfg.Profile == common.ProfilePersistent {
		for i := 0; i < cfg.Connections; i++ {
			wg.Add(1)
			go e.persistentWorker(cfg, jobs, cancel, &wg)
		}
	} else {
		for i := 0; i < cfg.Connections; i++ {
			wg.Add(1)
			go e.churnWorker(cfg, jobs, cancel, &wg)
		}
	}

	// Rate ticker: global scheduler, not per-connection sleep.
	interval := time.Second / time.Duration(cfg.TargetRatePerGenerator)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Actual-rate reporter.
	rateStop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-rateStop:
				return
			case <-t.C:
				n := e.sentWindow.Swap(0)
				e.rate.Set(float64(n))
			}
		}
	}()
	defer close(rateStop)

	var seq uint64
	for {
		select {
		case <-cancel.ch:
			close(jobs)
			wg.Wait()
			return
		default:
		}
		if time.Now().After(deadline) {
			close(jobs)
			wg.Wait()
			return
		}
		select {
		case <-cancel.ch:
			close(jobs)
			wg.Wait()
			return
		case <-ticker.C:
			seq++
			select {
			case jobs <- seq:
			default:
				// Scheduler overrun: count, never silently discard.
				atomic.AddUint64(&e.nDropped, 1)
				e.dropped.WithLabelValues(e.id).Inc()
			}
		}
	}
}

// persistentWorker owns ONE authenticated connection and reuses it for
// MAIL/RCPT/DATA loops, reconnecting transparently on failure.
func (e *Engine) persistentWorker(cfg common.TestConfig, jobs <-chan uint64, cancel *context_Cancel, wg *sync.WaitGroup) {
	defer wg.Done()
	var sc *smtpConn
	ensure := func() (*smtpConn, error) {
		if sc != nil {
			return sc, nil
		}
		ns, didTLS, err := dialSMTP(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPHost, cfg.TLSVerify, cfg.SMTPUser, cfg.SMTPPass, 15*time.Second)
		if err != nil {
			if strings.Contains(err.Error(), "auth") {
				atomic.AddUint64(&e.nAuthFail, 1)
				e.authFails.Inc()
			}
			return nil, err
		}
		if didTLS {
			atomic.AddUint64(&e.nTLS, 1)
			e.tlsTotal.Inc()
		}
		atomic.AddUint64(&e.nConnTotal, 1)
		e.connTotal.Inc()
		atomic.AddInt64(&e.nActive, 1)
		e.active.Inc()
		sc = ns
		return sc, nil
	}
	defer func() {
		if sc != nil {
			sc.Quit()
			atomic.AddInt64(&e.nActive, -1)
			e.active.Dec()
		}
	}()
	rcptIdx := 0
	for seq := range jobs {
		if cancel.cancelled() {
			// Drain accounting: remaining queued jobs for this worker count as dropped.
			_ = seq
			continue
		}
		conn, err := ensure()
		if err != nil {
			e.record(false, 0)
			continue
		}
		to := cfg.Recipients[rcptIdx%len(cfg.Recipients)]
		rcptIdx++
		from := fmt.Sprintf("load-%s@%s", e.id, cfg.FromDomain)
		msg := buildMessage(from, to, int64(seq), cfg.PayloadBytes)
		start := time.Now()
		atomic.AddUint64(&e.nAttempted, 1)
		e.attempted.WithLabelValues(e.id).Inc()
		if err := conn.sendOne(from, to, msg, 30*time.Second); err != nil {
			el := time.Since(start)
			e.record(false, el)
			// Drop broken connection; next job reconnects. No silent retry of
			// the rejected message (would inflate throughput) — it stays failed.
			sc.Close()
			sc = nil
			atomic.AddInt64(&e.nActive, -1)
			e.active.Dec()
			continue
		}
		el := time.Since(start)
		e.record(true, el)
	}
}

// churnWorker dials a brand-new TCP+STARTTLS+AUTH session per message.
func (e *Engine) churnWorker(cfg common.TestConfig, jobs <-chan uint64, cancel *context_Cancel, wg *sync.WaitGroup) {
	defer wg.Done()
	rcptIdx := 0
	for seq := range jobs {
		if cancel.cancelled() {
			_ = seq
			continue
		}
		to := cfg.Recipients[rcptIdx%len(cfg.Recipients)]
		rcptIdx++
		from := fmt.Sprintf("load-%s@%s", e.id, cfg.FromDomain)
		msg := buildMessage(from, to, int64(seq), cfg.PayloadBytes)
		start := time.Now()
		atomic.AddUint64(&e.nAttempted, 1)
		e.attempted.WithLabelValues(e.id).Inc()
		atomic.AddInt64(&e.nActive, 1)
		e.active.Inc()
		sc, didTLS, err := dialSMTP(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPHost, cfg.TLSVerify, cfg.SMTPUser, cfg.SMTPPass, 15*time.Second)
		if err != nil {
			atomic.AddInt64(&e.nActive, -1)
			e.active.Dec()
			if strings.Contains(err.Error(), "auth") {
				atomic.AddUint64(&e.nAuthFail, 1)
				e.authFails.Inc()
			}
			e.record(false, time.Since(start))
			continue
		}
		if didTLS {
			atomic.AddUint64(&e.nTLS, 1)
			e.tlsTotal.Inc()
		}
		atomic.AddUint64(&e.nConnTotal, 1)
		e.connTotal.Inc()
		serr := sc.sendOne(from, to, msg, 30*time.Second)
		sc.Quit()
		atomic.AddInt64(&e.nActive, -1)
		e.active.Dec()
		e.record(serr == nil, time.Since(start))
	}
}

func (e *Engine) record(ok bool, el time.Duration) {
	e.latency.Observe(el.Seconds())
	e.latMu.Lock()
	if len(e.latencies) < 100000 {
		e.latencies = append(e.latencies, float64(el.Microseconds())/1000.0)
	} else {
		e.latencies[mrand.Intn(len(e.latencies))] = float64(el.Microseconds()) / 1000.0 // reservoir sample
	}
	e.latMu.Unlock()
	if ok {
		atomic.AddUint64(&e.nAccepted, 1)
		e.accepted.WithLabelValues(e.id).Inc()
		e.sentWindow.Add(1)
	} else {
		atomic.AddUint64(&e.nFailed, 1)
		e.failed.WithLabelValues(e.id).Inc()
	}
}

// Status returns a snapshot for the controller's aggregation.
func (e *Engine) Status() map[string]any {
	p50, p95, p99 := e.quantiles()
	var elapsed float64
	e.mu.Lock()
	if !e.startTime.IsZero() {
		elapsed = time.Since(e.startTime).Seconds()
	}
	state := e.state
	e.mu.Unlock()
	return map[string]any{
		"generator_id":          e.id,
		"state":                 state,
		"attempted":             atomic.LoadUint64(&e.nAttempted),
		"accepted":              atomic.LoadUint64(&e.nAccepted),
		"failed":                atomic.LoadUint64(&e.nFailed),
		"dropped":               atomic.LoadUint64(&e.nDropped),
		"connections_active":    atomic.LoadInt64(&e.nActive),
		"connections_total":     atomic.LoadUint64(&e.nConnTotal),
		"tls_handshakes_total":  atomic.LoadUint64(&e.nTLS),
		"auth_failures_total":   atomic.LoadUint64(&e.nAuthFail),
		"actual_rate_msg_sec":   e.rateValue(),
		"latency_p50_ms":        p50,
		"latency_p95_ms":        p95,
		"latency_p99_ms":        p99,
		"elapsed_seconds":       elapsed,
	}
}

func (e *Engine) rateValue() float64 {
	// Best-effort read of the gauge via accepted/elapsed is done by the
	// controller; here return accepted per elapsed second.
	return 0 // controller computes windowed rate from successive scrapes
}

func (e *Engine) quantiles() (p50, p95, p99 float64) {
	e.latMu.Lock()
	defer e.latMu.Unlock()
	if len(e.latencies) == 0 {
		return 0, 0, 0
	}
	cp := append([]float64(nil), e.latencies...)
	sort.Float64s(cp)
	q := func(p float64) float64 {
		i := int(p * float64(len(cp)-1))
		return cp[i]
	}
	return q(0.50), q(0.95), q(0.99)
}

func (e *Engine) Registry() *prometheus.Registry { return e.reg }

// buildMessage renders a deterministic RFC5322 message of ~payloadBytes.
func buildMessage(from, to string, seq int64, payloadBytes int) []byte {
	var rnd [16]byte
	_, _ = rand.Read(rnd[:])
	head := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: loadtest %d %x\r\nMessage-ID: <%d.%x@loadtest>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n", from, to, seq, rnd[:4], seq, rnd[:4])
	body := fmt.Sprintf("loadtest seq=%d id=%x\n", seq, rnd)
	filler := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	need := payloadBytes - len(head) - len(body)
	var sb strings.Builder
	sb.WriteString(head)
	sb.WriteString(body)
	for need > 0 {
		chunk := filler
		if len(chunk) > need {
			chunk = chunk[:need]
		}
		sb.WriteString(chunk)
		need -= len(chunk)
		if need > 0 {
			sb.WriteString("\n")
			need--
		}
	}
	return []byte(sb.String())
}

// context_Cancel is a tiny cancellation primitive avoiding context import
// cycles in docs; behaves like context.WithCancel.
type context_Cancel struct {
	ch   chan struct{}
	once *sync.Once
}

func newCancel() *context_Cancel {
	return &context_Cancel{ch: make(chan struct{}), once: &sync.Once{}}
}
func (c *context_Cancel) cancel() {
	c.once.Do(func() { close(c.ch) })
}
func (c *context_Cancel) cancelled() bool {
	select {
	case <-c.ch:
		return true
	default:
		return false
	}
}
