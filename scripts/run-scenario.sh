#!/bin/sh
# Thin wrapper -- prefer calling run-scenario.py directly for full options:
#   python3 scripts/run-scenario.py scenarios/scenario-b.yaml --env-file .env --dry-run
exec python3 "$(dirname "$0")/run-scenario.py" "$@"
