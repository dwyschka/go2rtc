package petkit

// Regression test for the shutdown SIGSEGV: go2rtc's Connection.Stop closes
// the Transport (this MBuffer, i.e. munmap) while the producer's read loop —
// or the petkit teardown itself — may still be touching the mapping. Faulting
// on unmapped memory is a crash, not an error, so Close must fence out every
// concurrent operation and later calls must get errClosed.
//
// Needs a writable /dev/shm (Linux); run e.g. in a Linux container:
//   go test -race -run TestMBufferCloseDuringConcurrentAccess ./pkg/petkit/

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestMBufferCloseDuringConcurrentAccess(t *testing.T) {
	const ringSize = 1 << 16
	f, err := os.OpenFile(shmPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Skipf("cannot create %s: %v (needs writable /dev/shm)", shmPath, err)
	}
	if err = f.Truncate(ctrlSize + ringSize); err != nil {
		_ = f.Close()
		t.Fatalf("truncate: %v", err)
	}
	_ = f.Close()
	defer os.Remove(shmPath)

	mb, err := OpenMBuffer()
	if err != nil {
		t.Fatalf("OpenMBuffer: %v", err)
	}

	reader, err := mb.CreateReader("close-test", false)
	if err != nil {
		t.Fatalf("CreateReader: %v", err)
	}
	if err = reader.SetFilter(audioOutType); err != nil {
		t.Fatalf("SetFilter: %v", err)
	}

	var wg sync.WaitGroup

	// Writer floods the ring until Close cuts it off.
	wg.Add(1)
	go func() {
		defer wg.Done()
		payload := make([]byte, 512)
		for i := uint32(0); ; i++ {
			if err := mb.WriteAudioFrame(payload, uint64(i), i); errors.Is(err, errClosed) {
				return
			}
		}
	}()

	// Reader consumes concurrently until Close cuts it off. Also exercises
	// Release on a closed mapping (the old crash site).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if _, err := reader.ReadFrame(20); errors.Is(err, errClosed) {
				reader.Release() // must be a safe no-op after Close
				return
			}
		}
	}()

	time.Sleep(100 * time.Millisecond) // let both loops hammer the mapping

	if err := mb.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := mb.Close(); err != nil {
		t.Errorf("second Close must be a no-op, got: %v", err)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader/writer goroutines did not exit after Close")
	}

	// Every post-Close operation must degrade to errClosed, never fault.
	if err := mb.WriteAudioFrame([]byte{1}, 0, 0); !errors.Is(err, errClosed) {
		t.Errorf("WriteAudioFrame after Close: got %v, want errClosed", err)
	}
	if _, err := mb.CreateReader("late", true); !errors.Is(err, errClosed) {
		t.Errorf("CreateReader after Close: got %v, want errClosed", err)
	}
	if got := mb.ActiveReaders(); len(got) != 1 || got[0] != errClosed.Error() {
		t.Errorf("ActiveReaders after Close: got %v", got)
	}
	if got := mb.WriteNum(); got != 0 {
		t.Errorf("WriteNum after Close: got %d, want 0", got)
	}
}
