package petkit

import (
	"encoding/binary"
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

// dispatchSend sends one control message to "/msg_dispatch_<dst>".
func dispatchSend(dst, msgID uint16, payload []byte) error {
	name := "/msg_dispatch_" + strconv.FormatUint(uint64(dst), 10)

	mqd, err := mqOpen(name)
	if err != nil {
		return err
	}
	defer unix.Close(int(mqd))

	msg := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint16(msg[0:], msgID)
	binary.LittleEndian.PutUint16(msg[2:], dispatchSrcModule)
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
// In the app another daemon opens the speak session when an Agora call starts;
// nothing does that for us, so we send them ourselves. All best-effort (they
// need mqueue write access, i.e. running as root) — never fatal.
const (
	dispatchMediaModule uint16 = 2
	msgSpeakStart       uint16 = 5
	msgSpeakerEnable    uint16 = 18
)

// startTalkback opens a speak session on the media daemon and fires agora's
// audio-flow pings. Best-effort: failures are ignored so the frame-write path
// still runs (the session may already be open).
func startTalkback() {
	var on [4]byte
	on[0] = 1
	_ = dispatchSend(dispatchMediaModule, msgSpeakerEnable, on[:]) // power speaker
	_ = dispatchSend(dispatchMediaModule, msgSpeakStart, nil)      // spawn auido-out reader
	_ = dispatchSend(10, 1, nil)                                   // agora ping (0x0a,1)
	_ = dispatchSend(13, 1, []byte{0, 0, 0, 0})                    // agora ping (0x0d,1,{0})
}

// mqOpen opens (creating if necessary) a POSIX message queue for writing,
// matching the attributes libbase uses so it interoperates whether or not the
// queue already exists.
func mqOpen(name string) (uintptr, error) {
	np, err := unix.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}
	namePtr := uintptr(unsafe.Pointer(np))

	// The camera pipeline already created "/msg_dispatch_1"; open the existing
	// queue first (no O_CREAT, no attr). This sidesteps the kernel's create-time
	// mqueue limits (fs.mqueue.msg_max / msgsize_max) and mode checks that can
	// reject an O_CREAT open even for root.
	mqd, _, errno := unix.Syscall6(
		unix.SYS_MQ_OPEN, namePtr,
		uintptr(unix.O_WRONLY|unix.O_NONBLOCK), 0, 0, 0, 0,
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
		uintptr(unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CREAT), 0777,
		uintptr(unsafe.Pointer(&attr)), 0, 0,
	)
	if errno != 0 {
		return 0, errno
	}
	return mqd, nil
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
