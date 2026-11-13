// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package execx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Logger writes timestamped command invocations and their raw output to disk.
type Logger struct {
	mu   sync.Mutex
	path string
	file *os.File
}

// DefaultLogDir returns the directory where installer logs are stored.
func DefaultLogDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	dir := filepath.Join(cache, "substrate-gke", "logs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// NewLogger creates a new log file. If path is empty, a timestamped file in
// the default cache log directory is created.
func NewLogger(path string) (*Logger, error) {
	if path == "" {
		dir, err := DefaultLogDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(dir, fmt.Sprintf("installer-%s.log", time.Now().Format("20060102-150405")))
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &Logger{path: path, file: f}, nil
}

// Path returns the filesystem path to the log file.
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close closes the underlying log file.
func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

// LogCommandStart records the start of an external command with its parameters.
func (l *Logger) LogCommandStart(spec Spec) {
	if l == nil || l.file == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := time.Now().Format("2006-01-02 15:04:05.000")
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n=== [%s] START: %s ===\n", ts, spec.Display))
	if spec.Label != "" {
		b.WriteString(fmt.Sprintf("Label: %s\n", spec.Label))
	}
	if spec.Dir != "" {
		b.WriteString(fmt.Sprintf("Dir:   %s\n", spec.Dir))
	}
	if len(spec.Argv) > 0 {
		b.WriteString(fmt.Sprintf("Argv:  %s\n", strings.Join(spec.Argv, " ")))
	}
	if len(spec.Env) > 0 {
		b.WriteString(fmt.Sprintf("Env:   %s\n", strings.Join(spec.Env, " ")))
	}
	b.WriteString("--- Output ---\n")
	_, _ = l.file.WriteString(b.String())
	_ = l.file.Sync()
}

// LogLine records one raw output line.
func (l *Logger) LogLine(line string) {
	if l == nil || l.file == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.file.WriteString(line + "\n")
}

// LogCommandEnd records the termination of an external command.
func (l *Logger) LogCommandEnd(spec Spec, err error, dur time.Duration) {
	if l == nil || l.file == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := time.Now().Format("2006-01-02 15:04:05.000")
	status := "SUCCESS"
	if err != nil {
		status = fmt.Sprintf("FAILED (%v)", err)
	}
	msg := fmt.Sprintf("=== [%s] END: %s [%s] (duration: %v) ===\n\n", ts, spec.Display, status, dur.Round(time.Millisecond))
	_, _ = l.file.WriteString(msg)
	_ = l.file.Sync()
}
