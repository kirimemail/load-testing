# Safety

- Dedicated test domain only (`loadtest.example`); controlled sink
  recipients; never real addresses.
- `max_duration_secs` caps every run; controller rejects configs where
  `duration_seconds > max_duration_secs`.
- `POST /api/tests/emergency-stop` aborts all generators immediately.
- TLS verification is ON by default (`tls_verify: true`).
- Rejected messages are counted as failed, never silently retried in a way
  that inflates throughput.
- Credentials: Docker secrets or protected `.env` (see `.env.example`);
  never committed, never logged.
- Clean Haraka queues after each run before comparing scenarios.
