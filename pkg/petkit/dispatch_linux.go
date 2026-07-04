package petkit

import (
	"encoding/binary"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The dispatch subsystem (libbase msgdispatch.c) delivers small control
// messages between the camera's processes over POSIX message queues named
// "/msg_dispatch_<module>". On stream start tserver tells module 1 which media
// plane/audio it wants by sending the media_type bitmask.
const (
	dispatchDstModule uint16 = 1 // camera media manager
	dispatchMsgID     uint16 = 1 // "set frame type" message id
	dispatchSrcModule uint16 = 0 // tserver never registers a src id -> 0

	mqMaxMsg  = 128 // mq_attr.mq_maxmsg
	mqMsgSize = 544 // mq_attr.mq_msgsize (0x220)
)

// sendMediaType opens "/msg_dispatch_<dst>" and sends one message telling the
// camera pipeline which plane/audio to produce. Wire format (little-endian):
//
//	[0:2] msg_id   [2:4] src   [4:8] media_type
func sendMediaType(dst, msgID, src uint16, mediaType uint32) error {
	name := "/msg_dispatch_" + strconv.FormatUint(uint64(dst), 10)

	mqd, err := mqOpen(name)
	if err != nil {
		return err
	}
	defer unix.Close(int(mqd))

	var msg [8]byte
	binary.LittleEndian.PutUint16(msg[0:], msgID)
	binary.LittleEndian.PutUint16(msg[2:], src)
	binary.LittleEndian.PutUint32(msg[4:], mediaType)

	return mqSend(mqd, msg[:])
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
