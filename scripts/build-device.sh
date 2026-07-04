#!/usr/bin/env bash
# Build the minimal on-device go2rtc for Petkit cameras (petkit source +
# RTSP/WebRTC/HLS/MP4/MJPEG outputs only). Strips debug info to shrink the
# binary. No UPX — its self-extracting stub segfaults on some ARM kernels.
#
# Usage:
#   scripts/build-device.sh            # build armhf + mipsle into dist/
#   scripts/build-device.sh arm        # armhf only
#   scripts/build-device.sh mipsle     # mipsle only
set -euo pipefail
export LC_ALL=C

cd "$(dirname "$0")/.."
OUT="dist"
mkdir -p "$OUT"

TAGS="petkit_min"
LDFLAGS="-s -w"

build() {
	local label="$1" goarch="$2" goarm="${3:-}"
	local bin="$OUT/go2rtc-petkit-$label"

	echo ">> building $label ($goarch${goarm:+ GOARM=$goarm})"
	rm -f "$bin"
	env GOOS=linux GOARCH="$goarch" ${goarm:+GOARM="$goarm"} \
		go build -tags "$TAGS" -ldflags "$LDFLAGS" -trimpath -o "$bin" .

	local raw
	raw=$(wc -c <"$bin")
	awk -v b="$raw" 'BEGIN{printf "   %d bytes (%.1f MB)\n", b, b/1048576}'
}

# GOARM=6 (VFPv2): the target SoC lacks VFPv3, so a GOARM=7 build segfaults on
# it. GOARM=6 runs on that device and every newer ARMv7.
target="${1:-all}"
case "$target" in
	arm)    build armhf  arm 6 ;;
	mipsle) build mipsle mipsle ;;
	all)    build armhf  arm 6; build mipsle mipsle ;;
	*) echo "unknown target: $target (use: arm | mipsle | all)"; exit 1 ;;
esac

echo ">> done -> $OUT/"
