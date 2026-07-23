# Petkit driver

On-device replacement for the Petkit camera **`tserver`** binary.

Petkit cameras (little-endian MIPS "D4" / ARM "D4SH" firmware) run a small
process called `tserver` that serves the camera's H.264/AAC stream over HTTP.
Internally `tserver` does **not** encode anything itself — it reads already-encoded
media frames out of a POSIX shared-memory ring buffer that the camera's media
pipeline publishes, muxes them into FLV/MPEG-TS, and writes them to an HTTP
socket.

This driver reimplements the *reader* half of `tserver` and plugs it straight
into go2rtc's producer graph, so the device can expose RTSP / WebRTC / HLS /
MP4 instead of the fixed FLV/TS HTTP endpoint — **without** running `tserver` at
all.

## Usage

Runs on the device (Linux). In `go2rtc.yaml`:

```yaml
streams:
  cam:      petkit://main          # main (HQ) video + audio
  cam_sub:  petkit://sub           # sub (LQ) video + audio
  cam_mute: petkit://main?audio=0  # main video only
```

The host component is ignored (the buffer is always local shared memory);
`petkit://main` and `petkit:main` are equivalent. `main.flv` / `main.ts` /
`sub.ts` paths are accepted too — the container suffix is stripped since the
container is now go2rtc's job.

### Firmware layout (`?layout=` / `PETKIT_LAYOUT`)

The per-frame descriptor in the ring is laid out differently on different camera
SoC families (see the table below). The shared-memory control block and reader
slots are identical everywhere; only the descriptor field offsets move. Select
the profile per source with `?layout=` or globally with the `PETKIT_LAYOUT`
environment variable. Default is `arm`.

```yaml
streams:
  cam:  petkit://main?layout=t7   # Ingenic-T7 (MIPS) firmware
  arm:  petkit://main             # ARM/AXERA firmware (default)
```

Talkback (browser mic → camera speaker) defaults to whether the profile's device
has a speaker: on for `arm`, off for `t7` (mic-only). Override per stream with
`?talkback=0|1` — e.g. `petkit://main?layout=t7&talkback=1` if a T7 variant does
have a speaker.

| Profile | Aliases | Descriptor | Notes |
| --- | --- | --- | --- |
| `arm` | `axera`, `d4sh` | `0x38` bytes, type@`0x20` flags@`0x22` | Default. AXERA/ARM firmware. |
| `t7` | `mips`, `ingenic`, `d4` | `0x35` bytes, type@`0x18` flags@`0x1A` | Ingenic-T7 (LE MIPS). Verified from `t7_libbase.so`. |

A wrong layout means the reader never matches a frame (no video); pick the one
that matches the device's `tserver`/`libbase.so`.

#### Per-field overrides (unknown device variants)

If a device matches neither built-in profile, tune the individual descriptor
offsets right in the URL — no code change or rebuild. Each takes a base profile
and overrides the fields that differ. All values accept decimal or `0x`-hex;
`-1` marks a field as absent. Out-of-range offsets (a field reaching past
`hdr_size`) are rejected at parse time.

```yaml
streams:
  # start from t7, but this unit's type_flags sit one word later
  cam: petkit://main?layout=t7&flags_off=0x1c
  # fully hand-described descriptor
  odd: petkit://main?hdr_size=0x40&size_off=4&flags_off=0x24&type_off=0x22
```

Override keys: `hdr_size`, `num_off`, `size_off`, `index_off`, `pts_off`,
`type_off`, `flags_off`, `sps_off`, `pps_off`. The two critical ones for "no
video" are `hdr_size` (must equal the descriptor size the firmware copies) and
`flags_off` (the media-type bitmask the reader filters on).

## How it works (reverse-engineered)

| Piece | Detail |
| --- | --- |
| Ring buffer | POSIX shm `/media_buffer_frame_buf`: a `0x3E8` control block + power-of-two ring (2 MiB Ingenic-T7 / 8 MiB ARM, discovered via `fstat`). |
| Reader slot | A `0x2C`-byte consumer slot (name `ts-server`) is claimed in the control block; a filter mask selects main/sub/audio frames. |
| Dispatch | On start, a message is sent to POSIX mqueue `/msg_dispatch_1` — `[msg_id u16=1][src u16=0][media_type u32]` — telling the pipeline which plane/audio to emit. |
| Frame header | Firmware-dependent (see `?layout=`). **ARM** `0x38` B: `num@0, size@4, index@8, pts_us@0x10, type@0x20 (1=I,2=P), flags@0x22 (bit0 audio, bit2 main, bit3 sub), sps@0x32, pps@0x34`. **T7** `0x35` B: same `num/size/index/pts`, `type@0x18, flags@0x1A, sps@0x2F, pps@0x31`. |
| Video | H.264 Annex-B → converted to AVCC for go2rtc. |
| Audio | AAC in ADTS → header stripped, raw AU forwarded. |
| Locking | The control block starts with a process-shared glibc `pthread_mutex_t`. We speak its low-level futex ("lll") protocol directly in pure Go (`mutex_linux.go`) — no cgo — so cross-compilation to mipsel/armhf stays a plain `go build`. |

`media_type` bitmask: `main = 4`, `sub = 8`, `+audio = 5 / 9`.

## Files

- `petkit.go` — URL parsing, frame-header decode, ring-wrap helper (portable, unit-tested).
- `layout.go` — per-firmware frame-descriptor profiles (`arm`/`t7`) + `?layout=`/`PETKIT_LAYOUT` selection (portable, unit-tested).
- `mbuffer_linux.go` — shm map, reader-slot registration, `mbuffer_read_frame` port.
- `mutex_linux.go` — process-shared futex mutex (glibc lll protocol).
- `dispatch_linux.go` — mqueue control message.
- `producer_linux.go` — `Dial` + go2rtc `core.Producer` wiring.
- `petkit_other.go` — non-Linux stub (`Dial` returns an error).

## Building for the device

```bash
GOOS=linux GOARCH=arm   GOARM=7 go build -o go2rtc-armhf   .   # D4SH (ARM)
GOOS=linux GOARCH=mipsle        go build -o go2rtc-mipsle  .   # D4 (MIPS)
```
