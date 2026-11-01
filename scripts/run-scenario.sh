#!/bin/sh
# Usage: ./scripts/run-scenario.sh scenarios/scenario-b.yaml http://controller:8081
set -eu
echo "scenario files are applied via the controller API; example:"
echo "  curl -X POST $2/api/tests/start -H 'Content-Type: application/json' -d @- < rendered.json"
echo "Render the YAML scenario into the API payload with your relay hosts/creds first."
