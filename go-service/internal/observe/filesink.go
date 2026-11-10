package observe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileSink appends records as JSON lines to one file per service start.
type FileSink struct {
	mu   sync.Mutex
	path string
}

func NewFileSink(dir, name string) (*FileSink, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileSink{path: filepath.Join(dir, name)}, nil
}

func (f *FileSink) Path() string { return f.path }

// Write appends every record; the step-1 log keeps resent events too.
func (f *FileSink) Write(records []Record) ([]Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	enc := json.NewEncoder(file)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			return nil, fmt.Errorf("write record %d: %w", rec.ReceiveSeq, err)
		}
	}
	return records, nil
}

func (f *FileSink) Ready() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return errNotReady
	}
	return file.Close()
}
