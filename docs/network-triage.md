# Network triage: generator-limited vs network-limited vs relay-limited

Do NOT interpret a generator-limited or network-limited result as relay
capacity. For every run record three separate values:

1. Measured end-to-end throughput (`accepted_msg_sec` from `/api/report`).
2. SMTP relay processing capacity (relay CPU, queue drain rate, response
   latency at p95 while interface utilization < 70%).
3. Confirmed throughput after dedicated network deployment (re-run B).

## Signals

| Signal | Network saturation | Relay saturation |
| --- | --- | --- |
| Generator `dropped_total` rising | yes (scheduler overrun) | no |
| `smtp_transaction_duration_seconds` p99 spikes, relay CPU low | likely | no |
| Interface utilization > 85% + retransmits rising | yes | — |
| Relay CPU > 80% + queue depth growing, net flat | no | yes |
| All four pairs degrade together | shared-network suspect | possible |

## Collection

- Per-generator: `sar -n DEV 1`, `ss -s`, `ethtool -S`, ping relay RTT.
- Per-relay: CPU/mem, `smtp acceptance rate`, response latency, queue
  depth, queue drain rate, disk I/O, TCP retransmits.
- Prometheus scrapes generators every 5s; relays via node_exporter.
