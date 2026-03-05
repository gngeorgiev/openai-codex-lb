package lb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type EventLogger struct {
	mu   sync.Mutex
	file *os.File
}

type eventRecord struct {
	TS     string         `json:"ts"`
	Event  string         `json:"event"`
	Fields map[string]any `json:"fields,omitempty"`
}

func OpenEventLogger(root string) (*EventLogger, error) {
	if root == "" {
		var err error
		root, err = DefaultRootDir()
		if err != nil {
			return nil, err
		}
	}
	dir := filepath.Join(root, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	path := filepath.Join(dir, "proxy.current.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	return &EventLogger{file: f}, nil
}

func (l *EventLogger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *EventLogger) Log(event string, fields map[string]any) {
	if l == nil || l.file == nil {
		return
	}
	record := eventRecord{
		TS:     time.Now().UTC().Format(time.RFC3339Nano),
		Event:  event,
		Fields: fields,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	_, _ = l.file.Write(append(data, '\n'))
}
