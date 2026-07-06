package petkit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// POSIX shared-memory objects live under /dev/shm on Linux; opening that path
// is equivalent to shm_open("/media_buffer_frame_buf", ...).
const shmPath = "/dev/shm/media_buffer_frame_buf"

// Fixed layout constants (identical across the MIPS and ARM builds — they are
// application-level, not kernel-ABI, values).
const (
	ctrlSize     = 0x3e8 // control block size; ring data begins here
	frameHdrSize = 0x38  // per-frame header size within the ring
	slotSize     = 0x2c  // reader slot size
	slotCount    = 20    // number of reader slots
	slotArrayOff = 0x2c  // control-block offset of the reader-slot array
	readerNameMax = 15   // max reader name length (strncpy(name, 0xf))
)

// Control-block field offsets (all uint32 unless noted).
const (
	offWriteNum = 0x18 // sequence number of newest committed frame
	offMinNum   = 0x1c // oldest still-available sequence number
	offDataLen  = 0x20 // bytes currently occupied in the ring
	offTailPos  = 0x24 // ring offset of the oldest frame
	offHeadPos  = 0x28 // ring offset of the write head
)

// audioOutType is the type_flags value (header +0x22) that the media daemon's
// "auido-out" reader filters on for talkback audio. Note this is bit1 (value 2),
// distinct from the capture-audio bit used elsewhere.
const audioOutType uint16 = 0x0002

// Reader-slot field offsets (relative to the slot base).
const (
	slotName       = 0x00 // char[16]
	slotIndex      = 0x10 // uint16, slot number (seeded by the producer)
	slotFilterMask = 0x12 // uint16, wanted media bits
	slotLastNum    = 0x14 // uint32, last frame sequence the reader consumed
	slotBufCap     = 0x1c // uint32, producer-side copy-out buffer size (unused here)
	slotBufPtr     = 0x20 // void*, producer-side buffer pointer (unused here)
	slotActive     = 0x24 // uint32, 1 = slot in use
	slotWantWakeup = 0x28 // uint32, reader sets 1 before blocking on the semaphore
)

// pollInterval is how often ReadFrame re-checks the ring for new frames while
// waiting. tserver blocks on a POSIX semaphore; we poll instead, which avoids
// reimplementing the sem_t layout and is harmless because we leave want_wakeup
// at 0 so the producer never tries to post to us.
const pollInterval = 4 * time.Millisecond

// lockTimeout bounds every mutex acquisition so a crashed producer holding the
// shared lock can't wedge us forever.
const lockTimeout = 2 * time.Second

var (
	errBadName   = errors.New("petkit: reader name must be 1..15 chars")
	errNoSlot    = errors.New("petkit: no free reader slot in ring buffer")
	errTooSmall  = errors.New("petkit: shared memory smaller than control block")
	errFrameSize = errors.New("petkit: frame size exceeds ring — buffer desync")
	errTimeout   = errors.New("petkit: read frame timeout")
)

// MBuffer is a mapping of the camera's shared-memory media ring.
type MBuffer struct {
	fd       int
	data     []byte    // full mmap: control block + ring
	ring     []byte    // data[ctrlSize:]
	ringSize uint32    // len(ring); power of two
	ringMask uint32    // ringSize - 1
	mu       *shmMutex // process-shared lock at data[0]
}

// OpenMBuffer maps the existing shared-memory ring created by the camera
// pipeline. It does not create the object — the pipeline owns its lifecycle.
func OpenMBuffer() (*MBuffer, error) {
	fd, err := unix.Open(shmPath, unix.O_RDWR, 0)
	if err != nil {
		if err == unix.EACCES {
			return nil, fmt.Errorf("petkit: open %s read-write: %w — run go2rtc as root "+
				"or the same user as the camera daemon", shmPath, err)
		}
		if err == unix.ENOENT {
			return nil, fmt.Errorf("petkit: %s does not exist: %w — the camera media "+
				"pipeline must be running to publish the ring", shmPath, err)
		}
		return nil, fmt.Errorf("petkit: open %s: %w", shmPath, err)
	}

	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("petkit: fstat %s: %w", shmPath, err)
	}
	size := int(st.Size)
	if size <= ctrlSize {
		_ = unix.Close(fd)
		return nil, errTooSmall
	}

	ringSize := uint32(size - ctrlSize)
	// The ring is a power of two in every known build (4 MiB MIPS / 8 MiB ARM);
	// the firmware masks offsets with ringSize-1. If this file's ring is not a
	// power of two our whole layout assumption is wrong for this firmware —
	// fail loudly here instead of computing garbage offsets and crashing.
	if ringSize == 0 || ringSize&(ringSize-1) != 0 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("petkit: unexpected ring geometry: size=%d, ring=%d not a power of two "+
			"— shared-memory layout differs from the reverse-engineered spec", size, ringSize)
	}

	data, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("petkit: mmap %s (%d bytes): %w", shmPath, size, err)
	}

	mb := &MBuffer{
		fd:       fd,
		data:     data,
		ring:     data[ctrlSize:],
		ringSize: ringSize,
		ringMask: ringSize - 1,
		mu:       newShmMutex(data),
	}
	return mb, nil
}

// Close unmaps the ring and closes the descriptor.
func (mb *MBuffer) Close() error {
	err := unix.Munmap(mb.data)
	if cerr := unix.Close(mb.fd); err == nil {
		err = cerr
	}
	return err
}

// --- typed access into the mapping --------------------------------------

func (mb *MBuffer) loadU32(off int) uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(&mb.data[off])))
}

func (mb *MBuffer) storeU32(off int, v uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&mb.data[off])), v)
}

func (mb *MBuffer) storeU16(off int, v uint16) {
	binary.LittleEndian.PutUint16(mb.data[off:], v)
}

// ringCopy copies n bytes out of the ring starting at byte offset off, handling
// wrap-around at the ring boundary.
func (mb *MBuffer) ringCopy(off, n uint32) []byte {
	return ringRead(mb.ring, off, n)
}

// ringWrite writes src into the ring starting at byte offset off, wrapping at
// the ring boundary.
func (mb *MBuffer) ringWrite(off uint32, src []byte) {
	n := uint32(len(src))
	if off+n <= mb.ringSize {
		copy(mb.ring[off:off+n], src)
	} else {
		first := mb.ringSize - off
		copy(mb.ring[off:mb.ringSize], src[:first])
		copy(mb.ring[:n-first], src[first:])
	}
}

// WriteAudioFrame appends one ADTS-AAC frame to the ring, tagged as talkback
// audio (type_flags=0x0002) so the media daemon's "auido-out" reader decodes it
// and plays it on the speaker. It is a direct port of libbase's
// mbuffer_write_frame: take the shared lock, evict oldest frames if the ring is
// full, write the 0x38 header + payload at head_pos, then advance the counters.
//
// We are a second writer on the same ring the camera uses for video; the shared
// mutex serialises us with it, and video readers skip our frames by media-type.
//
// The media reader wakes on its own 500 ms poll, so no semaphore post is needed
// for continuous audio (only the very first frame after an idle gap can incur up
// to 500 ms latency).
// A nil/empty payload writes a header-only frame, which the media daemon reads
// as the end-of-stream marker (matches agora's save_audio_out_stop_frame).
func (mb *MBuffer) WriteAudioFrame(aac []byte, ptsUs uint64, frameIndex uint32) error {
	size := uint32(len(aac))
	if size+frameHdrSize > mb.ringSize {
		return errFrameSize
	}

	if err := mb.mu.Lock(lockTimeout); err != nil {
		return err
	}
	defer mb.mu.Unlock()

	writeNum := mb.loadU32(offWriteNum)
	minNum := mb.loadU32(offMinNum)
	dataLen := mb.loadU32(offDataLen)
	tailPos := mb.loadU32(offTailPos)
	headPos := mb.loadU32(offHeadPos)

	// Sanity reset if the control block looks corrupt.
	if headPos > mb.ringMask || tailPos > mb.ringMask || dataLen > mb.ringSize {
		writeNum, minNum, dataLen, tailPos, headPos = 0, 0, 0, 0, 0
	}

	// Evict oldest frames until the new one fits.
	for size+frameHdrSize+dataLen > mb.ringSize {
		raw := mb.ringCopy(tailPos, frameHdrSize)
		oldSize := binary.LittleEndian.Uint32(raw[0x04:])
		oldNum := binary.LittleEndian.Uint32(raw[0x00:])
		if dataLen < oldSize+frameHdrSize || oldNum != minNum {
			// Ring inconsistent — reset to empty.
			writeNum, minNum, dataLen, tailPos, headPos = 0, 0, 0, 0, 0
			break
		}
		tailPos = (tailPos + oldSize + frameHdrSize) & mb.ringMask
		dataLen -= oldSize + frameHdrSize
		minNum = oldNum + 1
	}

	writeNum++
	// Frame descriptor filled exactly like agora's __on_audio_data (the app's
	// proven talkback writer): audio frames carry a codec tag + sample-count so
	// the media daemon configures its decoder correctly.
	var hdr [frameHdrSize]byte
	binary.LittleEndian.PutUint32(hdr[0x00:], writeNum)              // num
	binary.LittleEndian.PutUint32(hdr[0x04:], size)                  // size
	binary.LittleEndian.PutUint32(hdr[0x0c:], uint32(time.Now().Unix())) // wall sec
	binary.LittleEndian.PutUint64(hdr[0x10:], ptsUs)                 // pts (us)
	binary.LittleEndian.PutUint64(hdr[0x18:], ptsUs)                 // local capture (us)
	hdr[0x20] = 0                                                    // frame_type
	hdr[0x21] = 4                                                    // codec = AAC
	binary.LittleEndian.PutUint16(hdr[0x22:], audioOutType)          // type_flags = 0x0002
	binary.LittleEndian.PutUint16(hdr[0x2e:], 0x0400)               // 1024 samples/frame (AAC-LC)
	binary.LittleEndian.PutUint16(hdr[0x30:], 0x0010)               // 16 (kHz/bit)
	_ = frameIndex

	if dataLen == 0 {
		minNum = writeNum
	}
	mb.ringWrite(headPos, hdr[:])
	headPos = (headPos + frameHdrSize) & mb.ringMask
	mb.ringWrite(headPos, aac)
	headPos = (headPos + size) & mb.ringMask
	dataLen += size + frameHdrSize

	mb.storeU32(offWriteNum, writeNum)
	mb.storeU32(offMinNum, minNum)
	mb.storeU32(offDataLen, dataLen)
	mb.storeU32(offTailPos, tailPos)
	mb.storeU32(offHeadPos, headPos)

	// Wake any reader parked in sem_timedwait whose filter matches this frame
	// (mirrors mbuffer_write_frame's per-slot wake loop). Without this the media
	// daemon's "auido-out" reader stays asleep and never plays our audio.
	for i := 0; i < slotCount; i++ {
		base := slotArrayOff + i*slotSize
		if mb.loadU32(base+slotActive) == 0 {
			continue
		}
		mask := binary.LittleEndian.Uint16(mb.data[base+slotFilterMask:])
		if mask&audioOutType == 0 {
			continue
		}
		if mb.loadU32(base+slotWantWakeup) == 0 {
			continue
		}
		idx := binary.LittleEndian.Uint16(mb.data[base+slotIndex:])
		mb.storeU32(base+slotWantWakeup, 0) // clear the gate before posting
		semPost(idx)
	}
	return nil
}

// ActiveReaders lists each registered reader slot as name(mask=0xNN). Used to
// confirm the media daemon started its "auido-out" audio reader (filter mask
// 0x0002) after we sent speak_start.
func (mb *MBuffer) ActiveReaders() []string {
	if err := mb.mu.Lock(lockTimeout); err != nil {
		return []string{"lock: " + err.Error()}
	}
	defer mb.mu.Unlock()

	var out []string
	for i := 0; i < slotCount; i++ {
		base := slotArrayOff + i*slotSize
		if mb.loadU32(base+slotActive) == 0 {
			continue
		}
		name := cstr(mb.data[base+slotName : base+slotName+16])
		mask := binary.LittleEndian.Uint16(mb.data[base+slotFilterMask:])
		wake := mb.loadU32(base + slotWantWakeup)
		lastNum := mb.loadU32(base + slotLastNum)
		out = append(out, fmt.Sprintf("%q(mask=0x%02x wake=%d num=%d)", name, mask, wake, lastNum))
	}
	return out
}

// WriteNum returns the ring's current newest-frame sequence number, for
// diagnostics (compare against a reader's num to see if it is keeping up).
func (mb *MBuffer) WriteNum() uint32 { return mb.loadU32(offWriteNum) }

// Reader is a registered consumer of the ring, mirroring tserver's "ts-server"
// reader. last-read bookkeeping is kept process-local (the producer never reads
// it); only the slot's name/index/filter/active/want_wakeup fields live in the
// shared control block.
type Reader struct {
	mb      *MBuffer
	slot    int    // slot index (0..19)
	slotOff int    // byte offset of the slot in the mapping
	filter  uint16 // wanted media bits
	lastNum uint32 // last consumed sequence number
	lastPos uint32 // current ring read offset
	lost    bool   // set when a frame gap was detected (ring lapped/underran)
}

// TakeLost reports whether frames were dropped since the last call and clears
// the flag. Callers use it to trigger a keyframe resync.
func (r *Reader) TakeLost() bool {
	lost := r.lost
	r.lost = false
	return lost
}

// CreateReader registers (or re-uses) a reader slot with the given name. When
// startNewest is true the reader begins at the live edge and skips any backlog
// (tserver uses flag=1); otherwise it starts from the oldest buffered frame.
func (mb *MBuffer) CreateReader(name string, startNewest bool) (*Reader, error) {
	if len(name) == 0 || len(name) > readerNameMax {
		return nil, errBadName
	}
	if err := mb.mu.Lock(lockTimeout); err != nil {
		return nil, err
	}
	defer mb.mu.Unlock()

	slot, free := -1, -1
	for i := 0; i < slotCount; i++ {
		base := slotArrayOff + i*slotSize
		active := mb.loadU32(base + slotActive)
		if active != 0 {
			if cstr(mb.data[base+slotName:base+slotName+16]) == name {
				slot = i
				break
			}
		} else if free < 0 {
			free = i
		}
	}
	if slot < 0 {
		slot = free
	}
	if slot < 0 {
		return nil, errNoSlot
	}

	base := slotArrayOff + slot*slotSize

	// Reset the slot to a clean registered state.
	for i := 0; i < 16; i++ {
		mb.data[base+slotName+i] = 0
	}
	copy(mb.data[base+slotName:base+slotName+readerNameMax], name)
	mb.storeU16(base+slotIndex, uint16(slot))
	mb.storeU16(base+slotFilterMask, 0)
	mb.storeU32(base+slotBufCap, 0) // we copy into Go memory, no producer buffer
	mb.storeU32(base+slotBufPtr, 0)
	mb.storeU32(base+slotWantWakeup, 0)
	mb.storeU32(base+slotActive, 1)

	r := &Reader{mb: mb, slot: slot, slotOff: base}
	if startNewest {
		r.lastNum = mb.loadU32(offWriteNum)
		r.lastPos = mb.loadU32(offHeadPos) & mb.ringMask
	} else {
		r.lastNum = mb.loadU32(offMinNum) - 1
		r.lastPos = mb.loadU32(offTailPos) & mb.ringMask
	}
	return r, nil
}

// SetFilter updates which media bits this reader accepts (main/sub/audio).
func (r *Reader) SetFilter(mask uint16) error {
	if err := r.mb.mu.Lock(lockTimeout); err != nil {
		return err
	}
	r.filter = mask
	r.mb.storeU16(r.slotOff+slotFilterMask, mask)
	r.mb.mu.Unlock()
	return nil
}

// Release marks the slot free so the producer stops emitting for it.
func (r *Reader) Release() {
	if err := r.mb.mu.Lock(lockTimeout); err != nil {
		return
	}
	r.mb.storeU32(r.slotOff+slotActive, 0)
	r.mb.mu.Unlock()
}

// ReadFrame returns the next frame matching the reader's filter, waiting up to
// timeoutMs milliseconds. It is a direct port of libbase's mbuffer_read_frame:
// take the shared lock, snap forward on ring loss, then scan committed frames.
func (r *Reader) ReadFrame(timeoutMs int) (*Frame, error) {
	mb := r.mb
	var deadline time.Time
	if timeoutMs > 0 {
		deadline = time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	}

	for {
		if err := mb.mu.Lock(lockTimeout); err != nil {
			return nil, err
		}

		writeNum := mb.loadU32(offWriteNum)
		minNum := mb.loadU32(offMinNum)
		tailPos := mb.loadU32(offTailPos) & mb.ringMask
		headPos := mb.loadU32(offHeadPos) & mb.ringMask

		// Loss BEFORE: we fell behind the oldest available frame.
		if int32((r.lastNum+1)-minNum) < 0 {
			r.lastNum = minNum - 1
			r.lastPos = tailPos
			r.lost = true
		}

		// Loss AFTER: the ring lapped past us; nothing valid to read now.
		if int32(writeNum-r.lastNum) < 0 {
			r.lastNum = writeNum
			r.lastPos = headPos
			r.lost = true
		} else {
			// Scan forward over committed frames.
			for int32(r.lastNum-writeNum) < 0 {
				raw := mb.ringCopy(r.lastPos, frameHdrSize)
				hdr := parseFrameHeader(raw)
				size := binary.LittleEndian.Uint32(raw[0x04:])
				if size == 0 || size > mb.ringSize {
					// Corrupt/desynced size: snap to the live edge and bail.
					r.lastNum = writeNum
					r.lastPos = headPos
					mb.mu.Unlock()
					return nil, errFrameSize
				}

				if hdr.Flags&r.filter != 0 {
					r.lastNum++
					dataOff := (r.lastPos + frameHdrSize) & mb.ringMask
					hdr.Data = mb.ringCopy(dataOff, size)
					r.lastPos = (dataOff + size) & mb.ringMask
					mb.mu.Unlock()
					return &hdr, nil
				}

				// Not our media type: skip the whole frame.
				r.lastNum = hdr.Num
				r.lastPos = (r.lastPos + frameHdrSize + size) & mb.ringMask
			}
		}

		mb.mu.Unlock()

		if timeoutMs == 0 {
			return nil, errTimeout
		}
		if time.Now().After(deadline) {
			return nil, errTimeout
		}
		time.Sleep(pollInterval)
	}
}
