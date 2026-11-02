package logging

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

// RotatingFile is an io.WriteCloser that rotates the underlying file once it
// would exceed maxBytes. Backups are named path.1 (newest) .. path.N (oldest).
// It exists because Termux has no logrotate/systemd-journald and the process
// may run for weeks.
type RotatingFile struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	f          *os.File
	size       int64
}

// NewRotatingFile opens (or creates) path in append mode.
func NewRotatingFile(path string, maxBytes int64, maxBackups int) (*RotatingFile, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("rotating file: maxBytes must be > 0, got %d", maxBytes)
	}
	if maxBackups < 0 {
		return nil, fmt.Errorf("rotating file: maxBackups must be >= 0, got %d", maxBackups)
	}
	r := &RotatingFile{path: path, maxBytes: maxBytes, maxBackups: maxBackups}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	r.f = f
	r.size = st.Size()
	return nil
}

// Write appends p, rotating first if p would push the file past maxBytes.
// A single write larger than maxBytes is written whole to a fresh file.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.f == nil {
		return 0, errors.New("rotating file: closed")
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return fmt.Errorf("close log file: %w", err)
	}
	r.f = nil

	if r.maxBackups == 0 {
		if err := os.Remove(r.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove log file: %w", err)
		}
	} else {
		for i := r.maxBackups - 1; i >= 1; i-- {
			src := fmt.Sprintf("%s.%d", r.path, i)
			dst := fmt.Sprintf("%s.%d", r.path, i+1)
			if err := os.Rename(src, dst); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("shift log backup: %w", err)
			}
		}
		if err := os.Rename(r.path, r.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate log file: %w", err)
		}
	}
	return r.open()
}

// Close flushes and closes the file. Safe to call more than once.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
