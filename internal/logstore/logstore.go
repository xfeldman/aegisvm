// Package logstore provides durable per-instance log storage with in-memory
// ring buffers and NDJSON file persistence.
package logstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxLines    = 10000
	maxBytes    = 5 * 1024 * 1024  // 5MB in-memory ring buffer
	maxFileBytes = 10 * 1024 * 1024 // 10MB per log file before rotation
)

// Log sources identify where a log entry originated.
const (
	SourceBoot   = "boot"   // Pre-serverReady boot output
	SourceServer = "server" // Main server process after ready
	SourceExec   = "exec"   // Exec command output
	SourceSystem = "system" // Lifecycle events (harness, demuxer)
)

// LogEntry represents a single log line from an instance.
type LogEntry struct {
	Timestamp  time.Time `json:"ts"`
	Stream     string    `json:"stream"`
	Line       string    `json:"line"`
	Source     string    `json:"source"`
	InstanceID string    `json:"instance_id"`
	AppID      string    `json:"app_id,omitempty"`
	ReleaseID  string    `json:"release_id,omitempty"`
	ExecID     string    `json:"exec_id,omitempty"`
}

// Store manages log storage for all instances.
type Store struct {
	mu      sync.RWMutex
	logs    map[string]*InstanceLog
	logsDir string
}

// NewStore creates a new log store, creating logsDir if needed.
func NewStore(logsDir string) *Store {
	os.MkdirAll(logsDir, 0700)
	return &Store{
		logs:    make(map[string]*InstanceLog),
		logsDir: logsDir,
	}
}

// GetOrCreate returns the InstanceLog for the given instance, creating it if needed.
func (s *Store) GetOrCreate(instanceID, appID, releaseID string) *InstanceLog {
	s.mu.RLock()
	il, ok := s.logs[instanceID]
	s.mu.RUnlock()
	if ok {
		return il
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock
	if il, ok := s.logs[instanceID]; ok {
		return il
	}

	filePath := filepath.Join(s.logsDir, instanceID+".ndjson")
	il = newInstanceLog(instanceID, appID, releaseID, filePath)
	s.logs[instanceID] = il
	return il
}

// Get returns the InstanceLog for the given instance, or nil if not found.
func (s *Store) Get(instanceID string) *InstanceLog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logs[instanceID]
}

// Remove closes the log for an instance and removes its files from disk.
func (s *Store) Remove(instanceID string) {
	s.mu.Lock()
	il, ok := s.logs[instanceID]
	if ok {
		delete(s.logs, instanceID)
	}
	s.mu.Unlock()

	if ok {
		il.Close()
		filePath := filepath.Join(s.logsDir, instanceID+".ndjson")
		os.Remove(filePath)
		os.Remove(filePath + ".1")
	}
}

// InstanceLog is a per-instance ring buffer with disk persistence and live subscriptions.
type InstanceLog struct {
	mu         sync.Mutex
	instanceID string
	appID      string
	releaseID  string

	// Ring buffer
	entries    []LogEntry
	head       int
	count      int
	totalBytes int

	// Subscribers
	subs []chan LogEntry

	// File persistence
	filePath  string
	file      *os.File
	fileBytes int64
}

func newInstanceLog(instanceID, appID, releaseID, filePath string) *InstanceLog {
	il := &InstanceLog{
		instanceID: instanceID,
		appID:      appID,
		releaseID:  releaseID,
		entries:    make([]LogEntry, maxLines),
		filePath:   filePath,
	}

	// Open or create log file
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		il.file = f
		info, _ := f.Stat()
		if info != nil {
			il.fileBytes = info.Size()
		}
	}

	return il
}

// Append adds a log entry to the ring buffer, persists to disk, and notifies subscribers.
func (il *InstanceLog) Append(stream, line, execID, source string) {
	entry := LogEntry{
		Timestamp:  time.Now(),
		Stream:     stream,
		Line:       line,
		Source:     source,
		InstanceID: il.instanceID,
		AppID:      il.appID,
		ReleaseID:  il.releaseID,
		ExecID:     execID,
	}

	il.mu.Lock()

	// Add to ring buffer, evicting oldest if necessary
	entrySize := len(line) + len(stream) + len(execID) + 100 // approximate overhead

	// Evict entries if over byte cap
	for il.count > 0 && il.totalBytes+entrySize > maxBytes {
		oldest := il.entries[il.head]
		oldSize := len(oldest.Line) + len(oldest.Stream) + len(oldest.ExecID) + 100
		il.totalBytes -= oldSize
		il.head = (il.head + 1) % maxLines
		il.count--
	}

	// Evict if at max lines
	if il.count >= maxLines {
		oldest := il.entries[il.head]
		oldSize := len(oldest.Line) + len(oldest.Stream) + len(oldest.ExecID) + 100
		il.totalBytes -= oldSize
		il.head = (il.head + 1) % maxLines
		il.count--
	}

	idx := (il.head + il.count) % maxLines
	il.entries[idx] = entry
	il.count++
	il.totalBytes += entrySize

	// Write to file
	if il.file != nil {
		data, err := json.Marshal(entry)
		if err == nil {
			data = append(data, '\n')
			n, err := il.file.Write(data)
			if err == nil {
				il.fileBytes += int64(n)
				// Rotate if file too large
				if il.fileBytes > maxFileBytes {
					il.rotate()
				}
			}
		}
	}

	// Copy subs slice to notify outside lock
	subs := make([]chan LogEntry, len(il.subs))
	copy(subs, il.subs)
	il.mu.Unlock()

	// Notify subscribers
	for _, ch := range subs {
		select {
		case ch <- entry:
		default:
		}
	}
}

func (il *InstanceLog) rotate() {
	if il.file != nil {
		il.file.Close()
	}
	os.Rename(il.filePath, il.filePath+".1")
	f, err := os.OpenFile(il.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		il.file = f
		il.fileBytes = 0
	}
}

// Read returns buffered entries filtered by since time, limited to last tail entries.
// If tail <= 0, all matching entries are returned.
func (il *InstanceLog) Read(since time.Time, tail int) []LogEntry {
	il.mu.Lock()
	defer il.mu.Unlock()

	var result []LogEntry
	for i := 0; i < il.count; i++ {
		idx := (il.head + i) % maxLines
		e := il.entries[idx]
		if !since.IsZero() && !e.Timestamp.After(since) {
			continue
		}
		result = append(result, e)
	}

	if tail > 0 && len(result) > tail {
		result = result[len(result)-tail:]
	}
	return result
}

// Subscribe returns a channel for live log entries, existing buffered entries,
// and an unsubscribe function. Pattern matches TaskStore.subscribeLogs.
func (il *InstanceLog) Subscribe() (ch chan LogEntry, existing []LogEntry, unsub func()) {
	il.mu.Lock()
	defer il.mu.Unlock()

	ch = make(chan LogEntry, 100)
	il.subs = append(il.subs, ch)

	// Snapshot existing entries
	existing = make([]LogEntry, 0, il.count)
	for i := 0; i < il.count; i++ {
		idx := (il.head + i) % maxLines
		existing = append(existing, il.entries[idx])
	}

	unsub = func() {
		il.mu.Lock()
		defer il.mu.Unlock()
		for i, s := range il.subs {
			if s == ch {
				il.subs = append(il.subs[:i], il.subs[i+1:]...)
				break
			}
		}
		close(ch)
	}

	return ch, existing, unsub
}

// Close closes the file handle.
func (il *InstanceLog) Close() {
	il.mu.Lock()
	defer il.mu.Unlock()
	if il.file != nil {
		il.file.Close()
		il.file = nil
	}
	// Close all subscriber channels
	for _, ch := range il.subs {
		close(ch)
	}
	il.subs = nil
}
