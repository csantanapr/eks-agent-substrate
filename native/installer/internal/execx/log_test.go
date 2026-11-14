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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoggerWritesCommandAndOutput(t *testing.T) {
	tmp := t.TempDir()
	logFile := filepath.Join(tmp, "test.log")

	logger, err := NewLogger(logFile)
	if err != nil {
		t.Fatalf("NewLogger failed: %v", err)
	}

	spec := Spec{
		Display: "echo hello",
		Dir:     tmp,
		Argv:    []string{"echo", "hello"},
		Env:     []string{"FOO=BAR"},
		Label:   "test step",
	}

	logger.LogCommandStart(spec)
	logger.LogLine("hello world line 1")
	logger.LogLine("error: connection refused")
	logger.LogCommandEnd(spec, errors.New("exit status 1"), 500*time.Millisecond)

	if err := logger.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	content := string(data)
	for _, expected := range []string{
		"START: echo hello",
		"Label: test step",
		"Dir:   " + tmp,
		"Env:   FOO=BAR",
		"hello world line 1",
		"error: connection refused",
		"END: echo hello [FAILED (exit status 1)]",
	} {
		if !strings.Contains(content, expected) {
			t.Errorf("log missing expected string %q, got content:\n%s", expected, content)
		}
	}
}

func TestDryRunWithLogger(t *testing.T) {
	tmp := t.TempDir()
	logFile := filepath.Join(tmp, "dryrun.log")

	logger, err := NewLogger(logFile)
	if err != nil {
		t.Fatalf("NewLogger failed: %v", err)
	}
	defer logger.Close()

	runner := DryRun{Delay: time.Millisecond, Log: logger}
	spec := Spec{
		Display:  "deploy thing",
		SimLines: []string{"line a", "line b"},
	}

	ch := runner.Start(context.Background(), spec)
	for ev := range ch {
		if ev.Done {
			break
		}
	}

	logger.Close()

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "START: deploy thing") || !strings.Contains(content, "line a") || !strings.Contains(content, "line b") {
		t.Errorf("unexpected dryrun log content:\n%s", content)
	}
}
