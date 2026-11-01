# Distributed SMTP Load Testing (Haraka)

Fleet load tester targeting **2,000 accepted SMTP msg/sec** (4 x 500) over
STARTTLS + SMTP AUTH, with persistent-connection and per-message-churn
profiles. Accepted and attempted rates are reported independently.

## Layout

| Path | Role |
| --- | --- |
| `cmd/generator` | Load generator (Go): SMTP engine + `:8080` mgmt API |
| `cmd/controller` | Fleet controller (Go): fan-out, aggregation, `:8081` API |
| `internal/common` | Shared `TestConfig` / status / report contract |
| `scenarios/` | Scenarios A–E (YAML) |
| `deploy/prometheus` | Scrape config (generators, controller, relay node_exporters) |
| `deploy/grafana` | Dashboard + provisioning |
| `docs/` | Network triage, safety |
| `Dockerfile.generator`, `Dockerfile.controller` | Images |
| `docker-compose.yml` | Local stack (controller + 1 gen + monitoring) |
| `docker-compose.remote-generator.yml` | One independent generator per load host |

## Quick start

```sh
cp .env.example .env   # fill TEST-relay creds; never commit
docker compose up -d --build
curl 127.0.0.1:8080/health
# configure + start fleet via controller:
curl -X POST 127.0.0.1:8081/api/tests/start \
  -H 'Content-Type: application/json' -d '{"name":"scenario-b","config":{...}}'
curl 127.0.0.1:8081/api/report   # accepted vs attempted + p50/p95/p99
curl -X POST 127.0.0.1:8081/api/tests/emergency-stop
```

## Deployment: single host vs multiple hosts

Both work, for different purposes.

### Single host (functional testing, CI, Scenario A)

`docker-compose.yml` runs controller + 1 generator + Prometheus + Grafana
on one machine. Good for validating wiring and baselining one relay
(Scenario A: 1 generator → 1 Haraka relay, 500 msg/sec). You can also run
several generator containers on one box for a dry run — but one NIC, one
kernel, and CPU contention will cap throughput, and the shared test network
caps it further. A single-host number is **not** Haraka capacity (see
`docs/network-triage.md`).

### Multiple hosts (real benchmarks, Scenarios B–E)

The brief requires 4 independent generators, preferably on separate
servers, paired 1:1 with the 4 Haraka relays. Never run generators on
the relay servers.

1. Clone this repo on each of the 4 load hosts.
2. On each host, start its generator with that host's identity
   (`docker-compose.remote-generator.yml` exists for exactly this;
   credentials come from a protected `.env`, never committed):

   ```sh
   GENERATOR_ID=gen-2 \
     docker compose -f docker-compose.remote-generator.yml up -d --build
   ```

3. On the controller host, point at all four private management
   addresses and start the fleet:

   ```sh
   GENERATORS="http://10.0.1.11:8080,http://10.0.1.12:8080,http://10.0.1.13:8080,http://10.0.1.14:8080" \
     docker compose up -d controller prometheus grafana
   curl -X POST 127.0.0.1:8081/api/tests/start \
     -H 'Content-Type: application/json' \
     -d '{"name":"scenario-b","config":{...}}'
   curl 127.0.0.1:8081/api/report
   ```

Rules that apply in both modes:

- 1 generator → 1 relay pairing (gen-N sends only to haraka-N),
  50 connections each in persistent mode.
- Management ports (8080/8081/9090/3000) bind to localhost — reach
  them over the private mgmt network or an SSH tunnel, never publicly.
- Keep SMTP test traffic and management traffic on separate networks
  where possible.

Practical path: validate single-host with Scenario A first, then roll
the same image out to the 4 load hosts for B–E.

## Generator API (`:8080`, private mgmt net only)

`POST /configure`, `POST /start`, `POST /stop`, `POST /emergency-stop`,
`GET /status`, `GET /metrics` (Prometheus), `GET /health`.

## Controller API (`:8081`, never public)

`POST /api/tests/start|stop|emergency-stop`, `GET /api/tests/status`,
`GET /api/report`.

## Metrics

`smtp_messages_attempted_total`,
`smtp_messages_accepted_total`, `smtp_messages_failed_total`,
`smtp_messages_dropped_total`, `smtp_connections_active`,
`smtp_connections_total`, `smtp_tls_handshakes_total`,
`smtp_auth_failures_total`, `smtp_transaction_duration_seconds`,
`smtp_generator_rate`.

## Safety

Dedicated test domain + sink recipients, `max_duration_secs` cap,
emergency stop, TLS verify on by default, no silent retries, no logged
passwords. See `docs/safety.md`. Report three values per run
(end-to-end, relay capacity, confirmed post-migration); see
`docs/network-triage.md`.
