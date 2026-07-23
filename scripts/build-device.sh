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
export CGO_ENABLED=0

# Pin Go 1.24.x for device builds. Go 1.25 added a fatal osinit check
# (runtime.getKernelVersion → configure64bitsTimeOn32BitsArchitectures) that
# throws when it can't parse the kernel's uname release string. The Petkit's
# Ingenic kernel reports "3.10.14__isvp_swan_1.0__", whose patch field isn't
# numeric, so a 1.25+ build dies at startup with a runtime throw (surfaced as
# "Trace/breakpoint trap"). 1.24.13 is the newest release that predates the
# check and still satisfies go.mod's `go 1.24.0`. Go auto-downloads it.
export GOTOOLCHAIN="${DEVICE_GOTOOLCHAIN:-go1.24.13}"

# NB: this builds STOCK (no GOMIPS override, no runtime overlay), exactly like
# the upstream go2rtc release binaries — which run on the Petkit T7 unchanged.
# That empirically settled two earlier over-corrections:
#   - rdhwr: the Ingenic kernel emulates the MIPS32r2 `rdhwr $29` used by Go's
#     TLS load_g, so no goroot overlay is needed. (An overlay that gated that
#     load actually BROKE the signal/g path -> the "Trace/breakpoint trap" we
#     chased.) The trap was the Go 1.25 uname bug above, not rdhwr.
#   - hardfloat: default GOMIPS=hardfloat runs fine (kernel provides FP), so no
#     softfloat override. If a future no-FPU unit needs it, add GOMIPS=softfloat
#     back for that target only.

# UPX-compress a built binary in place (~4x smaller — matters on the device's
# tiny flash). MIPS ONLY: the upstream go2rtc mipsle release is UPX+LZMA and
# runs on the Petkit T7 unchanged, so the self-extractor is fine here. NOT for
# ARM — its UPX stub segfaults on that kernel (leave armhf uncompressed).
#
# UPX VERSION MATTERS: upx >= 5.x emits a mipsel decompressor stub that uses a
# MIPS32r2 instruction the Ingenic XBurst r1 core traps on ("Trace/breakpoint
# trap" before Go even starts). upx 4.2.x (what upstream go2rtc packed with) is
# r1-safe. To stay deterministic regardless of the host's upx version, we ALWAYS
# pack inside a cached Docker image pinned to upx-ucl 4.2.x. Skip with NO_UPX=1;
# if Docker is unavailable the binary ships uncompressed.
UPX_IMAGE="go2rtc-upx:4"

upx_image_ready() {
	docker image inspect "$UPX_IMAGE" >/dev/null 2>&1 && return 0
	echo "   (building $UPX_IMAGE with upx-ucl 4.2.x — one-time)"
	docker build -q -t "$UPX_IMAGE" - >/dev/null <<-'EOF'
		FROM ubuntu:24.04
		RUN apt-get update && apt-get install -y --no-install-recommends upx-ucl \
		    && rm -rf /var/lib/apt/lists/*
	EOF
}

maybe_compress() {
	local bin="$1" goarch="$2"
	case "$goarch" in mips*) ;; *) return 0 ;; esac
	[ "${NO_UPX:-0}" = "1" ] && { echo "   (upx skipped: NO_UPX=1)"; return 0; }

	if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
		echo "   (Docker unavailable — shipping uncompressed; set NO_UPX=1 to silence)"
		return 0
	fi

	echo "   (packing with $UPX_IMAGE — r1-safe upx 4.2.x)"
	upx_image_ready
	docker run --rm -v "$PWD/$OUT":/w -w /w "$UPX_IMAGE" \
		sh -c "upx --best --lzma -q '$(basename "$bin")' >/dev/null && upx -t '$(basename "$bin")' >/dev/null" \
		|| { echo "!! containerized upx failed for $bin" >&2; exit 1; }
}

build() {
	local label="$1" goarch="$2" goarm="${3:-}"
	local bin="$OUT/go2rtc-petkit-$label"

	echo ">> building $label ($goarch${goarm:+ GOARM=$goarm})"
	rm -f "$bin"
	env GOOS=linux GOARCH="$goarch" ${goarm:+GOARM="$goarm"} \
		go build -tags "$TAGS" -ldflags "$LDFLAGS" -trimpath -o "$bin" .

	maybe_compress "$bin" "$goarch"

	local raw
	raw=$(wc -c <"$bin")
	awk -v b="$raw" 'BEGIN{printf "   %d bytes (%.1f MB)\n", b, b/1048576}'
}

# GOARM=6 (VFPv2): the target SoC lacks VFPv3, so a GOARM=7 build segfaults on
# it. GOARM=6 runs on that device and every newer ARMv7.
target="${1:-all}"
case "$target" in
	arm)    build armhf  arm 7 ;;
	mipsle) build mipsle mipsle ;;
	all)    build armhf  arm 7; build mipsle mipsle ;;
	*) echo "unknown target: $target (use: arm | mipsle | all)"; exit 1 ;;
esac

echo ">> done -> $OUT/"
