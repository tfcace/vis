#!/bin/sh
# Build cacheproxy for Linux, put it in a scratch image, and run it with a CPU limit,
# with the load generator beside it (vis-loadgen, sharing its network, idle until
# told a rate on 127.0.0.1:8792).
#
#   ./run-container.sh [cpus]         start it (default 2 CPUs); the proxy on 127.0.0.1:8790, MCP on 127.0.0.1:7070,
#                                     control UI on http://127.0.0.1:7071 (acting as VIS_UI_USER, default $USER)
#   VIS_UI_HOSTS=192.168.1.20 ./run-container.sh   let the control UI answer to that Host too, for a forwarder on the
#                                     LAN address. It has no login: whoever reaches it acts as VIS_UI_USER.
#                                     (set VIS_TOKENS=token=kind:id[:act],... to allow acting over MCP)
#   CACHEPROXY_FAULTS=1 ./run-container.sh      also serve the fault controls on 127.0.0.1:8793 (eval only)
#   docker update --cpus 0.25 vis-cacheproxy    squeeze it while it runs
#   docker rm -f vis-loadgen vis-cacheproxy     stop them
#
# It uses the current docker context (OrbStack here); set DOCKER_CONTEXT to pick another.
set -eu
cd "$(dirname "$0")"
arch=$(docker version --format '{{.Server.Arch}}')
mkdir -p build/linux
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o build/linux/cacheproxy .
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o build/linux/loadgen ./loadgen
docker build -q -t vis-cacheproxy -f Dockerfile build/linux >/dev/null
docker build -q -t vis-loadgen -f loadgen/Dockerfile build/linux >/dev/null
docker rm -f vis-loadgen vis-cacheproxy $(docker ps -aq --filter name='^vis-cp-') >/dev/null 2>&1 || true # the fleet too: they share ports
faults=""; [ "${CACHEPROXY_FAULTS:-}" = 1 ] && faults="-faults 0.0.0.0:8793"
# Publish on localhost only: the control UI has no login, and MCP needs tokens to act.
# shellcheck disable=SC2086 # $faults is two words on purpose
docker run -d --name vis-cacheproxy --cpus "${1:-2}" -e VIS_TOKENS="${VIS_TOKENS:-}" -e VIS_UI_USER="${VIS_UI_USER:-${USER:-local}}" \
	-e VIS_UI_HOSTS="${VIS_UI_HOSTS:-}" -p 127.0.0.1:8790:8790 -p 127.0.0.1:7070:7070 -p 127.0.0.1:7071:7071 -p 127.0.0.1:8793:8793 -p 127.0.0.1:8792:8792 \
	vis-cacheproxy $faults >/dev/null
docker run -d --name vis-loadgen --network container:vis-cacheproxy vis-loadgen >/dev/null
until curl -s -m 2 -o /dev/null 127.0.0.1:7071/api/state; do sleep 0.2; done
until curl -s -m 2 -o /dev/null 127.0.0.1:8792/stats; do sleep 0.2; done
[ -n "$faults" ] && until curl -sf -m 2 -o /dev/null 127.0.0.1:8793/faults; do sleep 0.2; done
echo "vis-cacheproxy is up with --cpus ${1:-2} on 127.0.0.1:8790, control UI on http://127.0.0.1:7071, load generator on 127.0.0.1:8792"
