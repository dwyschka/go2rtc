package petkit

import (
	"errors"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// shmMutex reimplements glibc's low-level lock ("lll") protocol for a
// PROCESS_SHARED, non-robust, non-PI pthread_mutex_t. The camera pipeline holds
// exactly such a mutex at offset 0 of the shared-memory ring; to interoperate
// with it byte-for-byte we speak the same futex protocol on its 32-bit lock
// word rather than using cgo/pthread.
//
// Lock word states (glibc __lll):
//
//	0 = unlocked
//	1 = locked, no waiters
//	2 = locked, one or more waiters
//
// The mutex is process-shared, so the futex operations must NOT set
// FUTEX_PRIVATE_FLAG.
type shmMutex struct {
	// addr points into the mmap'd shared memory at the mutex lock word
	// (pthread_mutex_t.__data.__lock, offset 0 on 32-bit glibc).
	addr *uint32
}

// classic futex op codes (stable kernel ABI, no private flag for shared mem).
const (
	futexWaitOp = 0 // FUTEX_WAIT
	futexWakeOp = 1 // FUTEX_WAKE
)

var errLockTimeout = errors.New("petkit: mutex lock timeout")

func newShmMutex(mapping []byte) *shmMutex {
	return &shmMutex{addr: (*uint32)(unsafe.Pointer(&mapping[0]))}
}

// Lock acquires the mutex, waiting up to timeout. A zero timeout waits forever.
func (m *shmMutex) Lock(timeout time.Duration) error {
	// Fast path: transition 0 -> 1.
	if atomic.CompareAndSwapUint32(m.addr, 0, 1) {
		return nil
	}

	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	// Contended path: mark as "locked with waiters" (2) and block on the futex
	// until the holder releases it. Mirrors glibc __lll_lock_wait.
	c := atomic.LoadUint32(m.addr)
	if c != 2 {
		c = atomic.SwapUint32(m.addr, 2)
	}
	for c != 0 {
		var waitFor time.Duration
		if timeout > 0 {
			waitFor = time.Until(deadline)
			if waitFor <= 0 {
				return errLockTimeout
			}
		}
		if err := futexWait(m.addr, 2, waitFor); err != nil {
			return err
		}
		c = atomic.SwapUint32(m.addr, 2)
	}
	return nil
}

// Unlock releases the mutex and wakes one waiter if there was contention.
func (m *shmMutex) Unlock() {
	// glibc __lll_unlock: if the old lock word was > 1 (i.e. 2, "locked with
	// waiters") there may be a waiter that needs waking.
	if atomic.SwapUint32(m.addr, 0) > 1 {
		futexWake(m.addr, 1)
	}
}

// futexWait blocks while *addr == val, up to timeout (0 = no timeout).
// Spurious wakeups, EAGAIN (value changed), EINTR and ETIMEDOUT are all
// non-errors — the caller re-checks the lock word.
func futexWait(addr *uint32, val uint32, timeout time.Duration) error {
	var tsp unsafe.Pointer
	if timeout > 0 {
		ts := unix.NsecToTimespec(timeout.Nanoseconds())
		tsp = unsafe.Pointer(&ts)
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)),
		uintptr(futexWaitOp),
		uintptr(val),
		uintptr(tsp),
		0, 0,
	)
	switch errno {
	case 0, unix.EAGAIN, unix.EINTR, unix.ETIMEDOUT:
		return nil
	default:
		return errno
	}
}

// futexWake wakes up to n waiters blocked on addr.
func futexWake(addr *uint32, n int) {
	_, _, _ = unix.Syscall6(
		unix.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)),
		uintptr(futexWakeOp),
		uintptr(n),
		0, 0, 0,
	)
}
