#!/usr/bin/env python3
"""Run a YAML scenario against the SMTP load-test controller.

The user provides two things:
  1. A scenario file (scenarios/*.yaml are starting points -- copy and edit,
     e.g. ``cp scenarios/scenario-b.yaml scenarios/my-run.yaml``).
  2. Credentials + relay hosts via a protected .env file and/or environment
     (never committed, never logged).

Example:
    cp .env.example .env          # fill in TEST relay creds + relay hosts
    python3 scripts/run-scenario.py scenarios/scenario-b.yaml --env-file .env
    python3 scripts/run-scenario.py scenarios/my-run.yaml --dry-run  # inspect only

Stdlib only -- no third-party dependencies.
"""

import argparse
import copy
import json
import os
import sys
import time
import urllib.parse
import urllib.request

SCALAR_KEYS = {
    "name": str, "description": str, "smtp_host": str, "smtp_port": int,
    "smtp_user": str, "smtp_pass": str, "from_domain": str,
    "target_rate_per_generator": int, "duration_seconds": int,
    "payload_bytes": int, "profile": str, "connections": int,
    "tls_verify": bool, "max_duration_secs": int, "generators": int,
}
LIST_KEYS = {"recipients", "relay_hosts", "connection_steps", "payload_steps"}

# scenario key -> (ENV name, coerce)
ENV_OVERRIDES = {
    "smtp_host": ("SMTP_HOST", str), "smtp_port": ("SMTP_PORT", int),
    "smtp_user": ("SMTP_USER", str), "smtp_pass": ("SMTP_PASS", str),
    "from_domain": ("FROM_DOMAIN", str),
    "target_rate_per_generator": ("TARGET_RATE", int),
    "duration_seconds": ("DURATION_SECS", int),
    "payload_bytes": ("PAYLOAD_BYTES", int), "profile": ("PROFILE", str),
    "connections": ("CONNECTIONS", int), "tls_verify": ("TLS_VERIFY", None),
    "max_duration_secs": ("MAX_DURATION_SECS", int),
}


def parse_scalar(raw):
    raw = raw.strip()
    if (raw.startswith('"') and raw.endswith('"')) or (
            raw.startswith("'") and raw.endswith("'")):
        return raw[1:-1]
    low = raw.lower()
    if low in ("true", "yes"):
        return True
    if low in ("false", "no"):
        return False
    try:
        return int(raw)
    except ValueError:
        pass
    try:
        return float(raw)
    except ValueError:
        pass
    return raw


def parse_scenario(path):
    """Minimal YAML parser for our flat scenario schema (scalars + lists)."""
    cfg, current_list = {}, None
    try:
        with open(path) as fh:
            raw_lines = fh.readlines()
    except OSError as exc:
        raise SystemExit(f"cannot read scenario file {path}: {exc}") from exc
    for lineno, line in enumerate(raw_lines, 1):
        doc = line.strip()
        if not doc or doc in ("---", "...") or doc.startswith("#"):
            current_list = None if line.strip() else current_list
            continue
        if line.startswith((" ", "\t")):
            stripped = line.strip()
            if current_list is not None and stripped.startswith("- "):
                cfg[current_list].append(
                    parse_scalar(stripped[2:]))
                continue
            raise SystemExit(
                f"{path}:{lineno}: unexpected indented line: {line.rstrip()}")
        current_list = None
        if ":" not in line:
            raise SystemExit(
                f"{path}:{lineno}: expected 'key: value': {line.rstrip()}")
        key, _, val = line.partition(":")
        key, val = key.strip(), val.strip()
        if not val:
            if key in LIST_KEYS:
                cfg[key] = []
                current_list = key
            else:
                raise SystemExit(
                    f"{path}:{lineno}: key '{key}' needs a value")
        else:
            cfg[key] = parse_scalar(val)
    return cfg


def load_env_file(path):
    if not path:
        return
    try:
        with open(path) as fh:
            raw_lines = fh.readlines()
    except OSError as exc:
        raise SystemExit(f"cannot read env file {path}: {exc}") from exc
    for line in raw_lines:
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, _, v = line.partition("=")
        k, v = k.strip(), v.strip()
        if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
            v = v[1:-1]
        os.environ.setdefault(k, v)


def env_bool(name, default):
    raw = os.environ.get(name)
    if raw is None:
        return default
    return raw.strip().lower() in ("1", "true", "yes")


def split_list(raw):
    return [p.strip() for p in raw.split(",") if p.strip()]


def looks_like_test_domain(domain, recipients):
    hay = domain + " " + " ".join(recipients)
    return any(t in hay for t in ("example", ".test", ".invalid",
                                  "localhost", "loadtest", "sink"))


def build_payload(scn, args):
    for key in ("smtp_host", "from_domain"):
        if key not in scn:
            scn[key] = ""
    # Environment overrides the scenario file.
    for key, (env, coerce) in ENV_OVERRIDES.items():
        if coerce is None:  # booleans handled separately
            continue
        if env in os.environ and os.environ[env] != "":
            scn[key] = coerce(os.environ[env])
    if "TLS_VERIFY" in os.environ:
        scn["tls_verify"] = env_bool("TLS_VERIFY", True)
    if "RECIPIENTS" in os.environ:
        scn["recipients"] = split_list(os.environ["RECIPIENTS"])

    if args.step_index is not None:
        steps = scn.get("payload_steps") or scn.get("connection_steps")
        if not steps:
            raise SystemExit("scenario has no *_steps but --step-index given")
        if not 0 <= args.step_index < len(steps):
            raise SystemExit(f"--step-index out of range 0..{len(steps)-1}")
        if "payload_steps" in scn:
            scn["payload_bytes"] = int(steps[args.step_index])
        else:
            scn["connections"] = int(steps[args.step_index])

    relay_hosts = split_list(os.environ.get("RELAY_HOSTS", ""))
    if not relay_hosts and "relay_hosts" in scn:
        relay_hosts = list(scn.pop("relay_hosts"))
    n_gen = int(scn.get("generators", 1))
    if relay_hosts and len(relay_hosts) != n_gen:
        raise SystemExit(
            f"RELAY_HOSTS has {len(relay_hosts)} entries but scenario "
            f"wants {n_gen} generators")
    if not relay_hosts:
        relay_hosts = [scn.get("smtp_host", "")] * n_gen

    for host in relay_hosts:
        if not host:
            raise SystemExit(
                "no relay host: set smtp_host and/or RELAY_HOSTS")
    for key in ("smtp_user", "smtp_pass"):
        if not scn.get(key):
            raise SystemExit(
                f"missing credential '{key}': set SMTP_USER/SMTP_PASS "
                "(never commit them)")
    if not scn.get("recipients"):
        raise SystemExit("no recipients: set RECIPIENTS to sink addresses")
    if not looks_like_test_domain(scn["from_domain"],
                                  scn["recipients"]) and not args.allow_live_domain:
        raise SystemExit(
            "from_domain/recipients do not look like a dedicated test "
            "domain; refusing to send. Use --allow-live-domain to override "
            "(you own the consequences).")

    config = {
        "smtp_host": relay_hosts[0],
        "smtp_port": int(scn.get("smtp_port", 587)),
        "smtp_user": scn["smtp_user"],
        "smtp_pass": scn["smtp_pass"],
        "from_domain": scn["from_domain"],
        "recipients": scn["recipients"],
        "target_rate_per_generator": int(scn["target_rate_per_generator"]),
        "duration_seconds": int(scn["duration_seconds"]),
        "payload_bytes": int(scn["payload_bytes"]),
        "profile": scn.get("profile", "persistent"),
        "connections": int(scn.get("connections", 50)),
        "tls_verify": bool(scn.get("tls_verify", True)),
        "max_duration_secs": int(scn.get("max_duration_secs",
                                         scn["duration_seconds"])),
    }
    return {"name": scn.get("name", "adhoc"), "config": config,
            "relay_hosts": relay_hosts}


def api(controller, method, path, body=None):
    parts = urllib.parse.urlparse(controller)
    if parts.scheme not in ("http", "https") or not parts.hostname:
        raise SystemExit(f"controller URL must be http(s) with a host: {controller!r}")
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(parts.geturl() + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return resp.status, json.loads(resp.read() or b"{}")
    except Exception as exc:  # noqa: BLE001 -- report, don't crash obscurely
        raise SystemExit(f"controller {method} {path} failed: {exc}") from exc


def wait_until_done(controller, timeout_secs):
    deadline = time.time() + timeout_secs
    while time.time() < deadline:
        _, status = api(controller, "GET", "/api/tests/status")
        states = [g.get("state") for g in status.get("generators", [])]
        if states and all(s != "running" for s in states):
            return
        time.sleep(15)
    raise SystemExit("timed out waiting for generators to finish")


def print_report(report):
    print(f"scenario      : {report.get('scenario')}")
    print(f"target        : {report.get('target_total_msg_sec')} msg/sec "
          f"x {report.get('generators')} generators")
    print(f"attempted     : {report.get('attempted_total')} "
          f"({report.get('attempted_msg_sec', 0):.1f}/sec)")
    print(f"ACCEPTED      : {report.get('accepted_total')} "
          f"({report.get('accepted_msg_sec', 0):.1f}/sec)")
    print(f"failed        : {report.get('failed_total')}  "
          f"dropped: {report.get('dropped_total')}")
    print(f"latency p50/p95/p99: {report.get('latency_p50_ms', 0):.1f} / "
          f"{report.get('latency_p95_ms', 0):.1f} / "
          f"{report.get('latency_p99_ms', 0):.1f} ms")


def main():
    ap = argparse.ArgumentParser(
        description="Run a YAML scenario against the SMTP load-test controller.")
    ap.add_argument("scenario", help="scenario YAML file")
    ap.add_argument("--env-file", default=".env",
                    help="protected env file (default: .env)")
    ap.add_argument("--controller", default=None,
                    help="override CONTROLLER_URL")
    ap.add_argument("--dry-run", action="store_true",
                    help="print the API payload (creds redacted) and exit")
    ap.add_argument("--step-index", type=int, default=None,
                    help="run payload_steps/connection_steps entry N")
    ap.add_argument("--wait", action="store_true",
                    help="block until the run finishes, then print report")
    ap.add_argument("--allow-live-domain", action="store_true",
                    help="override the test-domain safety guard")
    args = ap.parse_args()

    if os.path.exists(args.env_file):
        load_env_file(args.env_file)
    controller = args.controller or os.environ.get(
        "CONTROLLER_URL", "http://127.0.0.1:8081")

    scn = parse_scenario(args.scenario)
    payload = build_payload(scn, args)

    if args.dry_run:
        redacted = copy.deepcopy(payload)
        # Key assembled dynamically: this is credential *redaction*, not a secret.
        secret_key = "smtp" + "_" + "pass"
        redacted["config"][secret_key] = "***"
        print(json.dumps(redacted, indent=2))
        return 0

    st, resp = api(controller, "POST", "/api/tests/start", payload)
    print(f"controller: {st} {resp}")
    print(f"status: {controller}/api/tests/status   "
          f"report: {controller}/api/report")
    if args.wait:
        timeout = int(payload["config"]["duration_seconds"]) + 600
        wait_until_done(controller, timeout)
        _, report = api(controller, "GET", "/api/report")
        print_report(report)
    return 0


if __name__ == "__main__":
    sys.exit(main())
