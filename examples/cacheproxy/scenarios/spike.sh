#!/bin/sh
# A scan-heavy tenant arrives at t=30s and leaves at t=70s: the origin overloads, the CPU doesn't.
set -eu
. "$(dirname "$0")/common.sh"
load 800 0
tick steady; tick steady; tick steady
echo "--- scan-heavy tenant arrives (600 rps of cold keys)"; load 800 600
tick spike; tick spike; tick spike; tick spike
echo "--- scan tenant leaves"; load 800 0
tick recover; tick recover; tick recover; tick recover; tick recover
load 0 0
