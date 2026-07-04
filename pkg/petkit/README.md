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

## How it works (reverse-engineered)

| Piece | Detail |
| --- | --- |
| Ring buffer | POSIX shm `/media_buffer_frame_buf`: a `0x3E8` control block + power-of-two ring (4 MiB MIPS / 8 MiB ARM, discovered via `fstat`). |
| Reader slot | A `0x2C`-byte consumer slot (name `ts-server`) is claimed in the control block; a filter mask selects main/sub/audio frames. |
| Dispatch | On start, a message is sent to POSIX mqueue `/msg_dispatch_1` — `[msg_id u16=1][src u16=0][media_type u32]` — telling the pipeline which plane/audio to emit. |
| Frame header | `0x38` bytes: `num@0, size@4, index@8, pts_us@0x10 (64-bit µs), type@0x20 (1=I,2=P), flags@0x22 (bit0 audio, bit2 main, bit3 sub), sps_len@0x32, pps_len@0x34`. |
| Video | H.264 Annex-B → converted to AVCC for go2rtc. |
| Audio | AAC in ADTS → header stripped, raw AU forwarded. |
| Locking | The control block starts with a process-shared glibc `pthread_mutex_t`. We speak its low-level futex ("lll") protocol directly in pure Go (`mutex_linux.go`) — no cgo — so cross-compilation to mipsel/armhf stays a plain `go build`. |

`media_type` bitmask: `main = 4`, `sub = 8`, `+audio = 5 / 9`.

## Files

- `petkit.go` — URL parsing, frame-header decode, ring-wrap helper (portable, unit-tested).
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
