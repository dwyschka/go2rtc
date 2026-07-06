package petkit

import (
	"strconv"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// semPost posts the POSIX named semaphore "media_buffer_reader_<idx>", waking a
// media-buffer reader parked in sem_timedwait.
//
// It reimplements glibc's 32-bit __new_sem_post directly against the semaphore's
// /dev/shm backing file, so no cgo/libc is needed. On 32-bit glibc the sem_t is:
//
//	offset 0: unsigned int value     (token count)
//	offset 4: int          private   (0 = process-shared -> shared futex)
//	offset 8: unsigned int nwaiters  (number of blocked waiters)
//
// sem_post = atomically add one token, then FUTEX_WAKE the value word if any
// waiter is blocked. Verified from agora_arm: the writer wakes readers this way
// on the "media_buffer_reader_%d" semaphore keyed by the reader's slot index.
func semPost(idx uint16) {
	path := "/dev/shm/sem.media_buffer_reader_" + strconv.FormatUint(uint64(idx), 10)

	fd, err := unix.Open(path, unix.O_RDWR, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)

	// sizeof(sem_t) is 16 on 32-bit; map that.
	data, err := unix.Mmap(fd, 0, 16, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return
	}
	defer unix.Munmap(data)

	value := (*uint32)(unsafe.Pointer(&data[0]))
	nwaiters := (*uint32)(unsafe.Pointer(&data[8]))

	atomic.AddUint32(value, 1)
	if atomic.LoadUint32(nwaiters) > 0 {
		futexWake(value, 1) // process-shared futex (private == 0)
	}
}
