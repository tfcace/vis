# Shared by the scenarios: start the load generator on this host if none is
# running (run-container.sh starts one beside the proxy), and print one
# timeline line every 10 seconds.
here=${SCENARIOS:-$(cd "$(dirname "$0")" && pwd)} # set SCENARIOS when sourcing from elsewhere
A=${CACHEPROXY:-http://127.0.0.1:8790}
L=${LOADGEN:-http://127.0.0.1:8792}
if ! curl -s -m 2 -o /dev/null "$L/stats"; then
	(cd "$here/.." && go build -o build/loadgen ./loadgen)
	"$here/../build/loadgen" -target "$A" >/dev/null 2>&1 &
	loadgen_pid=$!
	trap 'kill $loadgen_pid 2>/dev/null' EXIT
	until curl -s -m 2 -o /dev/null "$L/stats"; do sleep 0.2; done
	curl -s -m 2 "$L/stats" >/dev/null # start its first window now
fi
load() { curl -s -m 5 -XPOST "$L/load" -d "{\"rps\":$1,\"scan_rps\":$2}" >/dev/null; }
tick() { sleep 10; t=$((t + 10)); "$here/line.py" "t=${t}s $1"; }
t=0
