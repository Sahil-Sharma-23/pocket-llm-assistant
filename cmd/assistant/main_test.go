package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunFlags(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	lan := filepath.Join(dir, "lan.json")
	mustWrite(t, good, `{"home": "`+filepath.ToSlash(dir)+`"}`)
	mustWrite(t, bad, `{"nonsense": true}`)
	mustWrite(t, lan, `{"server": {"addr": "0.0.0.0:8787"}}`)

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"version", []string{"-version"}, exitOK, "assistant dev", ""},
		{"check good config", []string{"-config", good, "-check-config"}, exitOK, filepath.Join(dir, "data"), ""},
		{"unknown field", []string{"-config", bad, "-check-config"}, exitConfig, "", "unknown field"},
		{"lan bind refused", []string{"-config", lan, "-check-config"}, exitConfig, "", "not a loopback"},
		{"missing file", []string{"-config", filepath.Join(dir, "nope.json")}, exitConfig, "", "read config"},
		{"bad flag", []string{"-bogus"}, exitConfig, "", "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PA_CONFIG", "")
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout %q missing %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr %q missing %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
