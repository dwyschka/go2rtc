# Petkit device support — fix log & layouts

This document is the running record of every fix made to the `petkit` driver,
grouped by subsystem, plus the reverse-engineered frame-descriptor layout for
each supported firmware. It complements `README.md` (which is the user-facing
setup guide): use `README.md` to configure a stream, use this file to understand
*why* the driver does what it does and what was broken on the way there.

Everything here is verified against the device's own binaries (`tserver`,
`libbase.so`, `agora_*`, `media_*`) in Ghidra — each entry names its ground-truth
source so a future regression can be re-checked against the same function.

---

## 1. Firmware layouts

The shared-memory transport (`/dev/shm/media_buffer_frame_buf`) is **byte-for-byte
identical across every firmware**: a `0x3E8` control block, a `0x2C`-byte reader
slot array at `+0x2C` (20 slots), and the same head/tail/num/dataLen counters.
The **only** thing that differs per SoC family is the per-frame descriptor that
precedes each payload in the ring. That descriptor is captured by `frameLayout`
in `layout.go`; pick it with `?layout=` on the stream URL or the `PETKIT_LAYOUT`
env var, or override individual offsets in the URL query (`hdr_size=`, `flags_off=`,
…) for a variant that matches no built-in profile.

The **ring size is not hardcoded** — it is derived from `fstat` at runtime and
must be a power of two — so only the descriptor offsets are pinned per firmware.

### Profile summary

| Profile | Aliases | Descriptor | Ring | Speaker / talkback | Ground truth |
| --- | --- | --- | --- | --- | --- |
| `arm` (default) | `axera`, `d4sh` | `0x38` bytes | 8 MiB | ✅ yes | `agora_arm`, `media_arm`, AXERA/ARM `tserver` |
| `t7` | `mips`, `mipsle`, `ingenic`, `d4` | `0x35` bytes | 2 MiB | ❌ mic only | `t7_libbase.so`, `t7_tserver` |
| `w7h` | `w7`, `w7hc` | `0x3D` bytes | 2 MiB | ✅ yes | `tserver_w7h`, `agora_w7h`, `media_w7h` |

### Field offsets

All fields little-endian. `-1` = the field does not exist in that firmware's
descriptor and is skipped by the writer. Widths: `num/size/index/wallSec` u32,
`pts/captureUS` u64, `type/codec` byte, `flags/sps/pps/samples/kHz` u16.

| Field | Meaning | `arm` (0x38) | `t7` (0x35) | `w7h` (0x3D) |
| --- | --- | --- | --- | --- |
| `offNum` | sequence number | `0x00` | `0x00` | `0x00` |
| `offSize` | payload length | `0x04` | `0x04` | `0x04` |
| `offIndex` | producer frame/slice counter | `0x08` | `0x08` | `0x08` |
| `offWallSec` | wall-clock seconds (write) | `0x0C` | `-1` | `0x0C` |
| `offPTS` | presentation time (µs) | `0x10` | `0x10` | `0x10` |
| `offCaptureUS` | local capture time (µs, write) | `0x18` | `-1` | `0x18` |
| `offType` | frame_type (1=I, 2=P) | `0x20` | `0x18` | `0x20` |
| `offCodec` | codec tag (4 = AAC, write) | `0x21` | `-1` | `0x21` |
| `offFlags` | type_flags media bitmask | `0x22` | `0x1A` | `0x22` |
| `offSamples` | samples/frame (0x400, write) | `0x2E` | `-1` | `0x33` |
| `offKHz` | sample-rate marker (0x10, write) | `0x30` | `-1` | `0x35` |
| `offSPS` | H.264 SPS length | `0x32` | `0x2F` | `0x37` |
| `offPPS` | H.264 PPS length | `0x34` | `0x31` | `0x39` |

`type_flags` bits (the reader's filter mask): **bit0** audio-capture, **bit1
(0x0002)** talkback audio-out, **bit2** main video, **bit3** sub video.

### Why the offsets differ

- **`arm` → `t7`:** the T7 descriptor has **no 64-bit capture timestamp** before
  the type/flags block, so every field from `+0x18` onward sits 8 bytes earlier
  than on ARM, and the whole descriptor is `0x35` (not `0x38`). The T7 is
  mic-only, so the ARM AAC-decoder-config fields (`codec`/`samples`/`kHz`) don't
  exist and the talkback writer omits them. Verified from `t7_libbase.so`
  (`mbuffer_read_frame`/`mbuffer_write_frame` copy `0x35`, filter at `+0x1A`,
  keyframe byte at `+0x18`; `avget_get_sample` in `t7_tserver` reads SPS/PPS at
  `+0x2F/+0x31`).
- **`arm` → `w7h`:** identical **through** `type_flags` at `+0x22`, then the whole
  tail (AAC hints + SPS/PPS) sits exactly **+5 bytes** later, making the
  descriptor `0x3D`. Verified from `tserver_w7h` `mbuffer_read_frame`
  (`FUN_0001bd00`, copies `0x3D`, ring masked `0x1FFFFF` = 2 MiB) and the frame
  consumer `FUN_00012214`; write side pinned from `agora_w7h` `__on_audio_data`
  (`FUN_00014a04`). The `w7h` media daemon's audio-out reader
  (`media_w7h FUN_0002d0d0`) ignores the sample hints and streams the payload
  straight into an `AX_ADEC` channel opened `enTransType=2` (TT_MP4_ADTS).

---

## 2. Talkback (browser mic → camera speaker)

Talkback was the hardest part of the driver and took the longest arc of fixes.
The path is: **WebRTC PCMA (G.711 A-law, 8 kHz) → PCM16 → 2× upsample → 16 kHz
mono → native AAC-LC encode → ADTS → shm ring (`type_flags=0x0002`) → media
daemon's "auido-out" reader → speaker.** The device wants 16 kHz mono ADTS-AAC.

### Fix log (newest first)

**Semaphore wake must not be gated on `want_wakeup` — steady ~¼–½ s lag**
*(uncommitted, this session — `mbuffer_linux.go`)*
The media daemon parks in `sem_timedwait` with a **500 ms** timeout and sets its
`want_wakeup` flag only just before parking. `WriteAudioFrame`'s wake loop
skipped the `sem_post` whenever it observed `want_wakeup == 0` — which, during
continuous speech (ring rarely empty), is most of the time. That's a lost-wakeup:
a frame written in the window *before* the reader set the flag left it asleep for
the full 500 ms even though its frame was already committed, adding a steady
0–500 ms (avg ~250 ms) of talkback lag. **Fix:** post the semaphore for every
audio frame on a filter match only, ungated. `semPost` is a counting semaphore —
if the reader isn't parked (`nwaiters==0`) it just bumps the token count (no
`FUTEX_WAKE`) and the reader drains that token on its next wait, so a frame is
never left waiting out the timeout.

**O(N log N) MDCT — talkback was seconds behind real time**
*(`6a6b34e`, 2026-07-09, `perf`)*
The direct-form MDCT cost ~2M float64 MACs per 64 ms frame plus a 64 KiB cosine
table that thrashed the Cortex-A L1 cache — over the real-time budget on the
camera's ARM core, so the audio sender's 128-packet queue filled and imposed a
constant ~2.5 s delay plus drops. **Fix:** standard fast MDCT — fold the 2048-tap
window into a 1024-point DCT-IV via one 512-point complex FFT with pre/post
rotations. Same transform, verified bin-for-bin against the direct form (kept as
the test oracle); 259× faster (1578 → 6.1 µs on M1), full `EncodeFrame` ~12 µs.
Also registered the talkback sender on the connection so `/api/streams` exposes
its packets/**drops** (drops > 0 = encoder slower than real time).

**Per-band AAC scalefactors — intelligible voice instead of buzz**
*(`c29aa66`, 2026-07-07, `feat`)*
With the ring/decode/speaker path verified clean (a real ADTS file plays
perfectly), the remaining "buzzy" audio was our own encoder: a single global
scalefactor set by the loudest bin collapsed quieter bands (formant + HF detail
that carry speech) into quantization noise — fine for a pure tone, bad for voice.
**Fix:** per-scalefactor-band quantization, DPCM-coded via the standard
scalefactor Huffman book; bands below 2% of the global peak are zeroed. Also
corrected the quantizer gain (`2^(-0.25·(sf-100))` inverse, the `^0.75` yields
the 0.1875) which only mattered once scalefactors varied per band.

**Mirror agora's trigger — drop volume/settings writes**
*(`12d3a2c`, 2026-07-07, `fix`)*
Talkback triggered a spoken "settings updated" prompt. RE of `agora_arm`
(`agora_rtsa_start_audio_play`) shows the speaker-open path sends **only**
`speak_start` (msg `0x0a`, dst 1, src 7, no payload) — never `set_volume`
(`0x14`), and `set_mic_volume` (`0x0d`) only conditionally for AEC. Our
`startTalkback` was writing device volume, which the cloud announces via
`cfg_update.aac`. **Fix:** send only `speak_start`; removed the `PETKIT_AO_VOL`
knob and the `0x0d/0x14` sends. The speaker already has a working volume from
`/opt/dev.conf`.

**Send ADTS + un-mute speaker**
*(`0542e6d`, 2026-07-07, `fix`)*
The speaker stayed silent while the ring reader *was* consuming frames. RE of
`media_arm` found two causes: (1) **wrong AAC format** — `AX_ADEC` channel 0 is
`enTransType=2` (TT_MP4_ADTS), `u32ConfLen=0`, so it needs self-describing ADTS;
raw AUs never sync and push a stale/empty frame to `AX_AO` (silence while frames
are consumed). Default to ADTS (`PETKIT_AAC_RAW=1` restores raw). (2) **speaker
muted** — boot sets `AX_AO_SetVqeVolume` from `ao_vol` in `/opt/dev.conf`;
un-mute via `dispatch_handler_set_volume` (msg `0x14`). *(The volume send here was
later removed by `12d3a2c` once ADTS turned out to be the real fix.)* Also flipped
the ARM build to `GOARM=7` (AXERA SoC has VFPv3).

**Wake mbuffer readers via `sem_post` — the missing piece**
*(`60777f0`, 2026-07-06, `feat`)*
Diagnostics proved the audio-out reader was parked in `sem_timedwait` (wake=1)
and its consumed frame num never advanced while `write_num` climbed — it was
never woken. **Fix:** reimplement glibc's 32-bit `__new_sem_post` in pure Go
against `/dev/shm/sem.media_buffer_reader_<idx>` (add a token at value@0,
`FUTEX_WAKE` if nwaiters@8 > 0), and wake every matching reader after each audio
frame — mirroring `agora_arm`'s `mbuffer_write_frame` wake loop. *(This is the
loop whose `want_wakeup` gate the current session's fix removes.)*

**Correct ADTS header strip**
*(`50d2f05`, 2026-07-06, `feat`)*
`aacPayload` now strips the correct ADTS header length (7 bytes, or 9 with CRC)
when raw-AU mode is selected.

**Correct ARM talkback trigger from agora ground truth**
*(`1655361`, 2026-07-06, `fix`)*
RE of `agora_arm` showed the real bug was the **trigger**, not the format: the
speaker is owned by media daemon **module 1** (not 2), and registers its ring
reader only after an "audio play" message — msg `0x0a` start / `0x0b` stop, sent
to `/msg_dispatch_1` with **src module 7** (impersonating agora). Our module-2 /
msg-5 hit nothing. `dispatchSendFrom` gained an explicit src.

**Correct talkback protocol from agora ground truth**
*(`c2501c7`, 2026-07-05, `fix`)*
First pass from `agora`'s `__on_audio_data`: removed a fatal `speak_start` (msg 5)
whose mqueue open failed `EACCES` and blocked the whole backchannel; filled the
full frame descriptor (codec=4 @`+0x21`, 1024 samples @`+0x2e`, `+0x30`=0x10,
capture timestamps @`+0x0c/+0x18`); write agora's zero-length end-of-stream frame
on stop. *(Later superseded by the module-1 `0x0a` trigger in `1655361`.)*

**Two-way audio + native AAC-LC encoder (initial)**
*(`a2a55f5`, 2026-07-05, `feat`)*
The talkback foundation: sendonly PCMA backchannel + `AddTrack`; G.711 → PCM16 →
2× upsample → 16 kHz → AAC-LC → ring via `WriteAudioFrame` (port of libbase
`mbuffer_write_frame`). Native cgo-free AAC-LC encoder (LUT MDCT initially,
single scalefactor, codebook 11 with escape/sign coding; Huffman tables from the
FAAC/ISO 14496-3 reference). Validated with ffmpeg recovering a 1 kHz tone.

### Diagnostic scaffolding built along the way

`petkit-talktest` (`6307315`) plays a sine or an ADTS file (`43a7061`) through
the talkback path without a browser; `TalkbackDiag`/`ActiveReaders` grew to
report reader slot index, filter mask, `want_wakeup`, last-consumed num, and the
live `/dev/shm/sem.*` files (`61f556a`, `50d2f05`, `94c9a1d`, `0515cda`) — the
instrumentation that pinned the parked-reader wake bug.

---

## 3. Dispatch (POSIX mqueue control messages)

**Strip leading slash from mqueue name — root `EACCES`**
*(`b6d1fb0`, 2026-07-06, `fix`)*
Our raw `SYS_mq_open` always got `EACCES`, even as root with no LSM. glibc strips
the leading `/` from a POSIX mqueue name before the syscall; the kernel's
`lookup_one_len` rejects any embedded `/`. **Fix:** pass `msg_dispatch_N` (no
slash) to the syscall. Unblocked every dispatch verb.

**Open dispatch mqueue `O_RDWR` (match libbase)**
*(`61ec786`, 2026-07-05, `fix`)*
Match `libbase` `open_mqueue` exactly (`O_RDWR|O_NONBLOCK`, flag `0x802`) instead
of `O_WRONLY`; richer per-queue errno diagnostics.

---

## 4. Stability

**SIGSEGV on producer stop — use-after-munmap of the shm ring**
*(`01dce29`, 2026-07-09, `fix`)*
`Producer.Stop` ran `Connection.Stop` first, which munmaps the ring; the teardown
that followed (talkback EOS frame, `stopTalkback`, `Reader.Release`) then
dereferenced the unmapped region, faulting inside the futex mutex. **Fix:**
`MBuffer` gained an `access` RWMutex + `closed` flag — every shm op holds it
shared, `Close` holds it exclusively, so `munmap` can never run under a concurrent
`ReadFrame`/`WriteAudioFrame`/`Release`; post-close calls degrade to `errClosed`;
`Close` is idempotent. `Producer.Stop` reordered: EOS + reader release first
(need the live mapping), `Connection.Stop` last. Regression test hammers `Close`
against concurrent reader+writer under `-race`.

---

## 5. Multi-firmware & build

**w7h firmware profile (ARM v8, talkback)**
*(uncommitted, `layout.go`)*
Added `layoutW7H` (`0x3D` descriptor, 2 MiB ring, speaker) + `w7h/w7/w7hc`
aliases. See §1 for offsets and ground truth.

**Configurable frame layout for Ingenic-T7/MIPS**
*(`335cef7`, 2026-07-23, `feat`)*
The driver hardcoded the ARM descriptor, so it read **no frames** on the T7
(wrong filter offset + wrong ring stride). Introduced `frameLayout` profiles
(`arm` default, `t7`) selectable via `?layout=` / `PETKIT_LAYOUT`, plus per-field
URL overrides (`hdr_size`/`flags_off`/…) with range validation so a new variant
needs only YAML. Added the `speaker` flag (defaults talkback off for the mic-only
T7). `WriteAudioFrame` writes a layout-sized descriptor and skips ARM-only fields.

**Stock MIPS build + containerized r1-safe UPX**
*(`81d4f17`, 2026-07-23, `build`)*
Dropped two over-corrections that broke the T7 (a goroot TLS overlay and
`GOMIPS=softfloat` — the stock hardfloat build runs; the real startup trap was
Go 1.25's `osinit` uname throw, avoided by the `GOTOOLCHAIN=go1.24.13` pin).
Compression: host `upx` 5.x emits a mipsel stub using a MIPS32r2 instruction the
Ingenic XBurst **r1** core traps on ("Trace/breakpoint trap"); pack inside a
cached Docker image pinned to `upx-ucl` 4.2.x (r1-safe). MIPS only; `NO_UPX=1`
skips; ships uncompressed if Docker is absent. 13 → 3 MB. Later `UPX_ARM=1`
opt-in added to also pack armhf (safe on the newer w7h kernel, still skipped for
the older AXERA whose kernel segfaults on the stub).

**Real-time timestamps in file playback test**
*(`f3a8ce2`, 2026-07-07, `fix`)*
File-mode `pts` started at 0 (ancient vs the device clock), so module 1 could
drop the frames as stale. Base `pts` on the wall clock.

---

## 6. Core (video)

**On-device tserver replacement via shared-memory ring**
*(`9d874d0`, 2026-07-05, `feat`)*
The foundation: read the camera's H.264/AAC frames directly from
`/media_buffer_frame_buf` (RE'd from `tserver`/`libbase`) and expose them through
go2rtc (RTSP/WebRTC/HLS/MP4/MJPEG), replacing the device's built-in tserver.
Includes the shm ring reader, a process-shared futex mutex speaking glibc's `lll`
protocol in pure Go (no cgo, so cross-compiling to mipsel/armhf stays a plain
`go build`), the mqueue dispatch, keyframe-resync + arrival-clock timestamps, the
`petkit://` source registration, and the `petkit_min` build tag for a minimal
on-device binary.

---

## Ground-truth reference index

| Binary | What it told us |
| --- | --- |
| `tserver`, `libbase.so` (ARM) | ring/control-block layout, `mbuffer_read/write_frame`, futex mutex |
| `t7_libbase.so`, `t7_tserver` | T7 `0x35` descriptor, filter@`0x1A`, SPS/PPS@`0x2F/0x31` |
| `tserver_w7h`, `agora_w7h`, `media_w7h` | w7h `0x3D` descriptor, `AX_ADEC enTransType=2` |
| `agora_arm` (`__on_audio_data`, `agora_rtsa_start_audio_play`) | talkback descriptor fields, module-1 `0x0a` trigger, `sem_post` wake loop |
| `media_arm` | `AX_ADEC` ADTS requirement, speaker un-mute path |
