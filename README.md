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

## Generator API (`:8080`, private mgmt net only)

`POST /configure`, `POST /start`, `POST /stop`, `POST /emergency-stop`,
`GET /status`, `GET /metrics` (Prometheus), `GET /health`.

## Controller API (`:8081`, never public)

`POST /api/tests/start|stop|emergency-stop`, `GET /api/tests/status`,
`GET /api/report`.

## Metrics

`surf_messages_attempted_total` — no: `smtp_messages_attempted_total`,
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
