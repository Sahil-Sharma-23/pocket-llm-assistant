package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"personal-assistant/internal/config"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"INFO", slog.LevelInfo, false},
		{"", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{" error ", slog.LevelError, false},
		{"trace", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseLevel(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewJSONToWriterFiltersLevel(t *testing.T) {
	var buf bytes.Buffer
	log, closeFn, err := New(config.LogConfig{Level: "warn", Format: "json"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	log.Info("hidden")
	log.Warn("shown", "k", "v")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["msg"] != "shown" || rec["k"] != "v" || rec["level"] != "WARN" {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestNewToFileCreatesDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "app.log")
	log, closeFn, err := New(config.LogConfig{Level: "info", Format: "text", File: path, MaxSizeMB: 1, MaxBackups: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello")
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "msg=hello") {
		t.Errorf("log file content = %q", b)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file perm = %o, want 600", perm)
	}
}

func TestRotatingFile(t *testing.T) {
	tests := []struct {
		name        string
		maxBackups  int
		writes      []string
		wantCurrent string
		wantBackups []string // index 0 = path.1
	}{
		{
			name:        "no rotation under limit",
			maxBackups:  2,
			writes:      []string{"aaaa", "bbbb"},
			wantCurrent: "aaaabbbb",
		},
		{
			name:        "rotates when exceeding",
			maxBackups:  2,
			writes:      []string{"aaaaaaa", "bbbbbbb"},
			wantCurrent: "bbbbbbb",
			wantBackups: []string{"aaaaaaa"},
		},
		{
			name:        "keeps at most maxBackups",
			maxBackups:  2,
			writes:      []string{"1111111", "2222222", "3333333", "4444444"},
			wantCurrent: "4444444",
			wantBackups: []string{"3333333", "2222222"},
		},
		{
			name:        "zero backups truncates",
			maxBackups:  0,
			writes:      []string{"1111111", "2222222"},
			wantCurrent: "2222222",
		},
		{
			name:        "oversized write goes to fresh file",
			maxBackups:  1,
			writes:      []string{"ab", "0123456789abcdef"},
			wantCurrent: "0123456789abcdef",
			wantBackups: []string{"ab"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "app.log")
			rf, err := NewRotatingFile(path, 10, tt.maxBackups)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.writes {
				if _, err := rf.Write([]byte(w)); err != nil {
					t.Fatal(err)
				}
			}
			if err := rf.Close(); err != nil {
				t.Fatal(err)
			}

			assertFile(t, path, tt.wantCurrent)
			for i, want := range tt.wantBackups {
				assertFile(t, fmt.Sprintf("%s.%d", path, i+1), want)
			}
			extra := fmt.Sprintf("%s.%d", path, len(tt.wantBackups)+1)
			if _, err := os.Stat(extra); !os.IsNotExist(err) {
				t.Errorf("unexpected backup %s (err=%v)", extra, err)
			}
		})
	}
}

func TestRotatingFileAppendsToExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("12345678"), 0o600); err != nil {
		t.Fatal(err)
	}
	rf, err := NewRotatingFile(path, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Existing 8 bytes + 4 > 10: must rotate, proving size was picked up.
	if _, err := rf.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	rf.Close()
	assertFile(t, path, "abcd")
	assertFile(t, path+".1", "12345678")
}

func TestRotatingFileWriteAfterClose(t *testing.T) {
	rf, err := NewRotatingFile(filepath.Join(t.TempDir(), "a.log"), 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	rf.Close()
	if err := rf.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := rf.Write([]byte("x")); err == nil {
		t.Error("expected error writing after Close")
	}
}

func TestNewRotatingFileRejectsBadArgs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.log")
	if _, err := NewRotatingFile(p, 0, 1); err == nil {
		t.Error("expected error for maxBytes=0")
	}
	if _, err := NewRotatingFile(p, 10, -1); err == nil {
		t.Error("expected error for maxBackups=-1")
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != want {
		t.Errorf("%s = %q, want %q", filepath.Base(path), b, want)
	}
}
