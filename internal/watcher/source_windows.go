//go:build windows

package watcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsRDCSource implements Source via ReadDirectoryChangesW with
// overlapped I/O. Robust to per-buffer overflow per BRD-03 §4.3:
// when ReadDirectoryChangesW reports a buffer overflow (the kernel had
// to drop events to keep up), the source emits a synthetic OpChange
// event for every file currently in the watched directory so the
// consumer's debouncer can re-hash and decide whether anything actually
// changed.
//
// Non-recursive in v0.5.1: bWatchSubtree = false; events for files
// outside the immediate directory are filtered.
type windowsRDCSource struct {
	dir       string
	handle    windows.Handle
	overlap   *windows.Overlapped
	event     windows.Handle // ManualResetEvent for completion
	buffer    []byte
	events    chan Event
	errors    chan error
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup // ensures runLoop exits before channels are closed
}

// rdcBufferSize is the buffer ReadDirectoryChangesW writes notifications
// into. Larger = fewer overflows on busy directories, more memory.
// 64 KB is a common practical default.
const rdcBufferSize = 64 * 1024

// waitTimeoutCode is the return value of WaitForSingleObject on timeout.
// WaitForSingleObject returns uint32; windows.WAIT_TIMEOUT is syscall.Errno
// so we use the raw value to avoid a type mismatch.
const waitTimeoutCode uint32 = 258 // 0x102

// New returns the Windows-native Source.
func New(dir string) (Source, error) {
	return newWindowsRDCSource(dir)
}

func newWindowsRDCSource(dir string) (Source, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("watcher/windows: resolve dir: %w", err)
	}
	pdir, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return nil, fmt.Errorf("watcher/windows: utf16: %w", err)
	}

	handle, err := windows.CreateFile(
		pdir,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("watcher/windows: CreateFile %s: %w", abs, err)
	}

	event, err := windows.CreateEvent(nil, 1 /* manual reset */, 0 /* initial state nonsignaled */, nil)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("watcher/windows: CreateEvent: %w", err)
	}

	s := &windowsRDCSource{
		dir:     abs,
		handle:  handle,
		overlap: &windows.Overlapped{HEvent: event},
		event:   event,
		buffer:  make([]byte, rdcBufferSize),
		events:  make(chan Event, 64),
		errors:  make(chan error, 8),
		done:    make(chan struct{}),
	}

	// Arm the FIRST ReadDirectoryChangesW synchronously, BEFORE New()
	// returns. Windows only records directory changes while an RDC call is
	// pending on the handle — a write that lands between construction and
	// the watch goroutine's first iteration would otherwise be lost forever.
	if err := s.arm(); err != nil {
		windows.CloseHandle(handle)
		windows.CloseHandle(event)
		return nil, fmt.Errorf("watcher/windows: ReadDirectoryChangesW: %w", err)
	}

	s.wg.Add(1)
	go s.runLoop()
	return s, nil
}

// arm queues exactly ONE overlapped ReadDirectoryChangesW. It must only be
// called when no IO is in flight on s.overlap — at construction and after a
// completed IO has been drained — because stacking RDC calls on the same
// OVERLAPPED is undefined (see the history note in runLoop).
func (s *windowsRDCSource) arm() error {
	const filter = windows.FILE_NOTIFY_CHANGE_FILE_NAME |
		windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_SIZE |
		windows.FILE_NOTIFY_CHANGE_LAST_WRITE |
		windows.FILE_NOTIFY_CHANGE_CREATION

	var bytesReturned uint32
	err := windows.ReadDirectoryChanges(
		s.handle,
		&s.buffer[0],
		uint32(len(s.buffer)),
		false, /* non-recursive per v0.5.1 */
		filter,
		&bytesReturned,
		s.overlap,
		0,
	)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		return err
	}
	return nil
}

func (s *windowsRDCSource) Add(dir string) error {
	return fmt.Errorf("watcher/windows: Source supports only the directory passed to New (v0.5.1 limitation)")
}

func (s *windowsRDCSource) Events() <-chan Event { return s.events }
func (s *windowsRDCSource) Errors() <-chan error { return s.errors }

func (s *windowsRDCSource) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		// Cancel any pending IO and free handles.
		_ = windows.CancelIoEx(s.handle, s.overlap)
		windows.CloseHandle(s.handle)
		windows.CloseHandle(s.event)
		// Wait for runLoop to exit before closing channels so no sender
		// writes to a closed channel (race safety via WaitGroup).
		s.wg.Wait()
		close(s.events)
		close(s.errors)
	})
	return nil
}

func (s *windowsRDCSource) runLoop() {
	defer s.wg.Done()

	// One overlapped ReadDirectoryChangesW is ALREADY in flight when this
	// goroutine starts — newWindowsRDCSource arms it synchronously so no
	// change can land in a gap before the first iteration. Each iteration
	// therefore waits for the in-flight IO, drains it, then re-arms exactly
	// ONE call at the bottom. (History: an earlier version re-queued RDC on
	// every WaitForSingleObject timeout, which silently cancelled the
	// in-flight IO on the same OVERLAPPED struct — Microsoft's docs say
	// stacking RDC calls on the same handle is undefined, and in practice
	// the kernel dropped whatever events were about to be written. arm()
	// must only ever run when no IO is pending.)
	for {
		select {
		case <-s.done:
			return
		default:
		}

		// Wait until the IO completes — or Close() fires CancelIoEx and
		// the wait wakes up via the event being signaled with an aborted
		// result. Poll done every 200 ms so we don't sit forever after
		// Close() if CancelIoEx didn't reach the kernel for some reason,
		// but do NOT re-queue RDC inside this inner loop.
		waited := false
		for !waited {
			select {
			case <-s.done:
				return
			default:
			}
			ev, werr := windows.WaitForSingleObject(s.event, 200 /* ms */)
			if werr != nil {
				continue
			}
			if ev == waitTimeoutCode {
				continue
			}
			waited = true
		}

		windows.ResetEvent(s.event)

		var transferred uint32
		err := windows.GetOverlappedResult(s.handle, s.overlap, &transferred, false)
		switch {
		case err != nil:
			if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
				return // Close() called
			}
			select {
			case s.errors <- fmt.Errorf("watcher/windows: GetOverlappedResult: %w", err):
			default:
			}
			// The IO completed (with an error) — fall through to re-arm so
			// the directory keeps being watched.
		case transferred == 0:
			// Buffer overflow (or zero events; treat as overflow defensively).
			s.handleOverflow()
		default:
			s.parseAndEmit(s.buffer[:transferred])
		}

		// Re-arm exactly one RDC for the next iteration. Safe: the previous
		// IO has fully completed and been drained above.
		if err := s.arm(); err != nil {
			select {
			case s.errors <- fmt.Errorf("watcher/windows: ReadDirectoryChangesW: %w", err):
			default:
			}
			return
		}
	}
}

// parseAndEmit walks the FILE_NOTIFY_INFORMATION records in buf and pushes
// Events to the channel. Each record has a NextEntryOffset; offset 0 means
// last record.
//
// fniHeaderSize is the on-the-wire size of FILE_NOTIFY_INFORMATION up to
// (but not including) the variable-length FileName: 3 × uint32 = 12 bytes.
// We must NOT use unsafe.Sizeof(windows.FileNotifyInformation{}) here —
// that returns 16 because the Go struct's [1]uint16 FileName placeholder
// is padded out to 4-byte alignment, but the kernel emits records that
// can be as small as 14 bytes for a single-character name. Using the
// padded value silently drops every single-character file/directory
// event (e.g. `mkdir x`) — which is exactly what made the recursive
// watcher fail on Windows CI for mkdir -p x/y/z chains.
const fniHeaderSize = 12

func (s *windowsRDCSource) parseAndEmit(buf []byte) {
	var offset uint32 = 0
	for {
		if int(offset)+fniHeaderSize > len(buf) {
			return
		}
		raw := (*windows.FileNotifyInformation)(unsafe.Pointer(&buf[offset]))

		nameLen := raw.FileNameLength
		// FileName is the first uint16 of the variable-length UTF-16LE name
		// field. Compute its byte offset within the struct via unsafe.Offsetof.
		nameStart := offset + uint32(unsafe.Offsetof(raw.FileName))
		nameBytes := buf[nameStart : nameStart+nameLen]
		// Decode UTF-16LE bytes into a []uint16 and then to a Go string.
		nameRunes := make([]uint16, nameLen/2)
		for i := uint32(0); i < nameLen/2; i++ {
			nameRunes[i] = uint16(nameBytes[i*2]) | uint16(nameBytes[i*2+1])<<8
		}
		name := windows.UTF16ToString(nameRunes)

		fullPath := filepath.Join(s.dir, name)
		op := OpChange
		switch raw.Action {
		case windows.FILE_ACTION_REMOVED, windows.FILE_ACTION_RENAMED_OLD_NAME:
			op = OpRemove
		}

		// Non-recursive filter — Windows can deliver subdirectory events
		// under certain configurations; double-check here too.
		if filepath.Dir(fullPath) == s.dir {
			select {
			case s.events <- Event{Path: fullPath, Op: op}:
			case <-s.done:
				return
			}
		}

		if raw.NextEntryOffset == 0 {
			return
		}
		offset += raw.NextEntryOffset
	}
}

// handleOverflow is called when ReadDirectoryChangesW indicates the kernel
// buffer overflowed. Emit a warning on Errors() and synthesize OpChange
// events for every file currently in the directory so the consumer's
// debouncer can re-hash and detect actual changes.
func (s *windowsRDCSource) handleOverflow() {
	select {
	case s.errors <- fmt.Errorf("watcher/windows: ReadDirectoryChangesW buffer overflow; re-scanning %s", s.dir):
	default:
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		select {
		case s.events <- Event{Path: filepath.Join(s.dir, e.Name()), Op: OpChange}:
		case <-s.done:
			return
		}
	}
}
