#!/bin/sh
# The container's CPU limit drops to a quarter core at t=30s and comes back at t=70s.
# Run cacheproxy with ../run-container.sh first. Uses DOCKER_CONTEXT if set.
set -eu
C=${CONTAINER:-vis-cacheproxy}
. "$(dirname "$0")/common.sh"
load 800 0
tick steady; tick steady; tick steady
echo "--- CPU limit squeezed to 0.25 cores"; docker update --cpus 0.25 "$C" >/dev/null
tick squeezed; tick squeezed; tick squeezed; tick squeezed
echo "--- CPU limit back to 2 cores"; docker update --cpus 2 "$C" >/dev/null
tick recover; tick recover; tick recover; tick recover; tick recover
load 0 0
echo "--- what vis changed:"
docker logs "$C" 2>&1 | grep 'msg=vis.change' | sed 's/^time=[^ ]* level=[^ ]* //' | cut -c1-200 || echo "nothing"
