package petkit

import (
	"encoding/binary"
	"os"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The dispatch subsystem (libbase msgdispatch.c) delivers small control
// messages between the camera's processes over POSIX message queues named
// "/msg_dispatch_<module>". Messages are little-endian: [0:2] msg_id, [2:4] src
// module (we are not registered -> 0), [4:] payload.
const (
	dispatchDstModule uint16 = 1 // camera media manager (video)
	dispatchMsgID     uint16 = 1 // "set frame type" message id
	dispatchSrcModule uint16 = 0 // we never register a src id -> 0

	mqMaxMsg  = 128 // mq_attr.mq_maxmsg
	mqMsgSize = 544 // mq_attr.mq_msgsize (0x220)
)

// dispatchSend sends one control message to "/msg_dispatch_<dst>" with src 0.
func dispatchSend(dst, msgID uint16, payload []byte) error {
	return dispatchSendFrom(dst, msgID, dispatchSrcModule, payload)
}

// dispatchSendFrom sends a control message with an explicit src module id.
func dispatchSendFrom(dst, msgID, src uint16, payload []byte) error {
	name := "/msg_dispatch_" + strconv.FormatUint(uint64(dst), 10)

	mqd, err := mqOpen(name)
	if err != nil {
		return err
	}
	defer unix.Close(int(mqd))

	msg := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint16(msg[0:], msgID)
	binary.LittleEndian.PutUint16(msg[2:], src)
	copy(msg[4:], payload)

	return mqSend(mqd, msg)
}

// sendMediaType tells the camera pipeline which plane/audio to produce.
func sendMediaType(dst, msgID, src uint16, mediaType uint32) error {
	var payload [4]byte
	binary.LittleEndian.PutUint32(payload[:], mediaType)
	return dispatchSend(dst, msgID, payload[:])
}

// The media/audio daemon (DISPATCH_RECEIVER_MEDIA, module 2) talkback verbs.
// Talkback control (verified from agora_arm on the actual ARM/AXERA device):
// the speaker is owned by media daemon module 1, which registers its ring
// reader only after an "audio play" message. agora sends it impersonating
// itself (src module 7). Start = msg 0x0a, stop = 0x0b, both to /msg_dispatch_1.
const (
	dispatchAudioModule uint16 = 1
	msgAudioStart       uint16 = 0x0a
	msgAudioStop        uint16 = 0x0b
	msgAudioFlag        uint16 = 0x0d
	msgAudioVolume      uint16 = 0x14 // dispatch_handler_set_volume: u32 level 0-9
	agoraSrcModule      uint16 = 7
)

// aoVolumeLevel is the speaker volume level (0-9, mapped to 0.1..1.0) we push on
// talkback start. Override with PETKIT_AO_VOL; default 9 (full volume).
var aoVolumeLevel = func() uint32 {
	if v, err := strconv.Atoi(os.Getenv("PETKIT_AO_VOL")); err == nil && v >= 0 && v <= 9 {
		return uint32(v)
	}
	return 9
}()

// startTalkback tells module 1 to start its speaker ring reader, exactly like
// agora's audio watchdog. Best-effort — failures are ignored.
func startTalkback() {
	_ = dispatchSendFrom(dispatchAudioModule, msgAudioFlag, agoraSrcModule, []byte{0, 0, 0, 0})
	_ = dispatchSendFrom(dispatchAudioModule, msgAudioStart, agoraSrcModule, nil)
	setSpeakerVolume(aoVolumeLevel)
}

// setSpeakerVolume un-mutes the speaker. The media daemon's boot init
// (media_arm FUN_00029380) sets AX_AO_SetVqeVolume from ao_vol in
// /opt/dev.conf; when that key is 0 or absent the speaker is muted (volume 0.0)
// and no PCM is audible even though frames decode fine — the reason talkback
// stayed silent while frames were being consumed. dispatch_handler_set_volume
// (msg 0x14, payload = u32 level 0-9) calls AX_AO_SetVqeVolume((level+1)*0.1),
// so any level yields >= 0.1 and lifts the mute. Best-effort.
func setSpeakerVolume(level uint32) {
	if level > 9 {
		level = 9
	}
	var payload [4]byte
	binary.LittleEndian.PutUint32(payload[:], level)
	_ = dispatchSendFrom(dispatchAudioModule, msgAudioVolume, agoraSrcModule, payload[:])
}

// stopTalkback tells module 1 to stop the speaker ring reader.
func stopTalkback() {
	_ = dispatchSendFrom(dispatchAudioModule, msgAudioStop, agoraSrcModule, nil)
}

// mqOpen opens (creating if necessary) a POSIX message queue for writing,
// matching the attributes libbase uses so it interoperates whether or not the
// queue already exists.
func mqOpen(name string) (uintptr, error) {
	np, err := unix.BytePtrFromString(mqSyscallName(name))
	if err != nil {
		return 0, err
	}
	namePtr := uintptr(unsafe.Pointer(np))

	// Open the existing queue first (no O_CREAT), exactly like libbase's
	// open_mqueue: O_RDWR|O_NONBLOCK (flag 0x802). This sidesteps create-time
	// mqueue limits and mode checks.
	mqd, _, errno := unix.Syscall6(
		unix.SYS_MQ_OPEN, namePtr,
		uintptr(unix.O_RDWR|unix.O_NONBLOCK), 0, 0, 0, 0,
	)
	if errno == 0 {
		return mqd, nil
	}
	if errno != unix.ENOENT {
		return 0, errno
	}

	// Queue doesn't exist yet — create it with the attributes libbase uses.
	// struct mq_attr on Linux: long mq_flags, mq_maxmsg, mq_msgsize,
	// mq_curmsgs, __reserved[4]. Go int == C long on all Linux GOARCHes.
	attr := [8]int{0, mqMaxMsg, mqMsgSize, 0, 0, 0, 0, 0}
	mqd, _, errno = unix.Syscall6(
		unix.SYS_MQ_OPEN, namePtr,
		uintptr(unix.O_RDWR|unix.O_NONBLOCK|unix.O_CREAT), 0777,
		uintptr(unsafe.Pointer(&attr)), 0, 0,
	)
	if errno != 0 {
		return 0, errno
	}
	return mqd, nil
}

// mqSyscallName strips the leading '/' from a POSIX mqueue name. glibc's
// mq_open does this before the raw SYS_mq_open syscall; the kernel's
// lookup_one_len rejects any embedded '/' with EACCES, so passing "/name"
// directly to the syscall fails for everyone, even root.
func mqSyscallName(name string) string {
	if len(name) > 0 && name[0] == '/' {
		return name[1:]
	}
	return name
}

// mqProbe attempts to open an existing queue (no create) for diagnostics and
// returns the raw errno (0 = success).
func mqProbe(name string) unix.Errno {
	np, err := unix.BytePtrFromString(mqSyscallName(name))
	if err != nil {
		return unix.EINVAL
	}
	mqd, _, errno := unix.Syscall6(
		unix.SYS_MQ_OPEN, uintptr(unsafe.Pointer(np)),
		uintptr(unix.O_RDWR|unix.O_NONBLOCK), 0, 0, 0, 0,
	)
	if errno == 0 {
		unix.Close(int(mqd))
	}
	return errno
}

// mqSend delivers one message at priority 0 (mq_timedsend with a NULL timeout;
// the queue is non-blocking, so a full queue returns EAGAIN rather than
// blocking).
func mqSend(mqd uintptr, msg []byte) error {
	_, _, errno := unix.Syscall6(
		unix.SYS_MQ_TIMEDSEND,
		mqd,
		uintptr(unsafe.Pointer(&msg[0])),
		uintptr(len(msg)),
		0, // msg_prio
		0, // NULL abs_timeout
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
