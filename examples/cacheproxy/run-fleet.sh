#!/bin/sh
# cacheproxy as a fleet: N replicas (default 3) on one Docker network, joined
# into one vis ring seeded from the first, each with its own CPU limit, and the
# load generator spreading requests across them as a load balancer would.
#
#   ./run-fleet.sh [n] [cpus]      start it (default 3 replicas of 2 CPUs each)
#
# Replica i (1..n) is container vis-cp-i, named cp-i in the ring. Its ports step
# by 10: MCP on 127.0.0.1:7070, 7080, 7090, ..., the control UI one above (7071,
# 7081, ...), and with CACHEPROXY_FAULTS=1 its fault controls on 8793, 8803, ...
# The load generator is on 127.0.0.1:8792.
#
#   docker update --cpus 0.25 vis-cp-2    squeeze one replica
#   docker network disconnect vis-peers vis-cp-3   cut cp-3 off its peers while clients still reach it
#
# Peers talk on a network of their own, vis-peers, as cp-i-peers: so a
# partition of the ring can leave the traffic flowing, as a real one can.
#   ./run-fleet.sh stop                   stop the fleet
#
# VIS_TOKENS, VIS_UI_USER and VIS_UI_HOSTS reach every replica, as in
# run-container.sh. Each start makes a new cluster key unless VIS_CLUSTER_KEY is set.
set -eu
cd "$(dirname "$0")"
stop() {
	docker rm -f vis-loadgen vis-cacheproxy $(docker ps -aq --filter name='^vis-cp-') >/dev/null 2>&1 || true
	docker network rm vis-fleet vis-peers >/dev/null 2>&1 || true
}
if [ "${1:-}" = stop ]; then stop; exit 0; fi
n=${1:-3}; cpus=${2:-2}
arch=$(docker version --format '{{.Server.Arch}}')
mkdir -p build/linux
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o build/linux/cacheproxy .
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o build/linux/loadgen ./loadgen
docker build -q -t vis-cacheproxy -f Dockerfile build/linux >/dev/null
docker build -q -t vis-loadgen -f loadgen/Dockerfile build/linux >/dev/null
stop # the single-container vehicle too: they share ports
docker network create vis-fleet >/dev/null
docker network create vis-peers >/dev/null
key=${VIS_CLUSTER_KEY:-$(openssl rand -hex 16)}
faults=""; [ "${CACHEPROXY_FAULTS:-}" = 1 ] && faults="-faults 0.0.0.0:8793"
targets=""
seeds=$(seq 1 "$n" | sed 's/.*/cp-&-peers:7946/' | paste -sd, -) # every replica: one down mustn't leave a restart alone
for i in $(seq 1 "$n"); do
	# Published on localhost only, as in run-container.sh: the UI has no login.
	# shellcheck disable=SC2086 # $faults is two words on purpose
	docker run -d --name "vis-cp-$i" --hostname "cp-$i" --network vis-fleet --cpus "$cpus" \
		-e VIS_SEEDS="$seeds" -e VIS_PEER_ADVERTISE="cp-$i-peers:7946" -e VIS_CLUSTER_KEY="$key" -e VIS_TOKENS="${VIS_TOKENS:-}" \
		-e VIS_UI_USER="${VIS_UI_USER:-${USER:-local}}" -e VIS_UI_HOSTS="${VIS_UI_HOSTS:-}" \
		-p "127.0.0.1:$((7070 + 10 * (i - 1))):7070" -p "127.0.0.1:$((7071 + 10 * (i - 1))):7071" -p "127.0.0.1:$((8793 + 10 * (i - 1))):8793" \
		vis-cacheproxy $faults >/dev/null
	docker network connect --alias "cp-$i-peers" vis-peers "vis-cp-$i"
	targets="$targets${targets:+,}http://cp-$i:8790"
done
docker run -d --name vis-loadgen --network vis-fleet -p 127.0.0.1:8792:8792 --entrypoint /loadgen vis-loadgen \
	-target "$targets" -control 0.0.0.0:8792 >/dev/null
for i in $(seq 1 "$n"); do
	until curl -s -m 2 -o /dev/null "127.0.0.1:$((7071 + 10 * (i - 1)))/api/state"; do sleep 0.2; done
done
until curl -s -m 2 -o /dev/null 127.0.0.1:8792/stats; do sleep 0.2; done
# joined: the first replica sees all n alive
until [ "$(curl -s -m 2 127.0.0.1:7071/api/state | python3 -c 'import json,sys; print(sum(m["state"]=="alive" for m in json.load(sys.stdin).get("fleet",{}).get("members",[])))' 2>/dev/null)" = "$n" ]; do sleep 0.5; done
echo "fleet of $n cacheproxy replicas (--cpus $cpus each) is up: cp-1's control UI on http://127.0.0.1:7071, MCP on 127.0.0.1:7070; load generator on 127.0.0.1:8792"
