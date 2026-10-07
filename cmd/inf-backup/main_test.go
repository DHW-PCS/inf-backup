package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestVersionHelpAndConfigurationFailure(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		code     int
		contains string
	}{
		{[]string{"--version"}, 0, "3.0.0"},
		{[]string{"--help"}, 0, "--config"},
		{[]string{"--config", filepath.Join(t.TempDir(), "missing.yml")}, 1, "配置错误"},
	} {
		var out bytes.Buffer
		if code := run(context.Background(), tc.args, strings.NewReader(""), &out); code != tc.code || !strings.Contains(out.String(), tc.contains) {
			t.Fatalf("code=%d output=%q", code, out.String())
		}
	}
}

type synchronizedOutput struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	progress chan struct{}
	once     sync.Once
}

func (o *synchronizedOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.buffer.Write(data)
	if strings.Contains(string(data), `"event":"progress"`) {
		o.once.Do(func() { close(o.progress) })
	}
	return n, err
}

func TestEOFAndSignalContextCleanUpAcceptedWork(t *testing.T) {
	for _, cause := range []string{"eof", "signal", "stop"} {
		t.Run(cause, func(t *testing.T) {
			root := t.TempDir()
			resticPath := filepath.Join(root, "offline-restic")
			script := "#!/bin/sh\nif [ \"$1\" = version ]; then printf 'restic 0.18.1\\n'; exit 0; fi\nprintf '%s\\n' '{\"message_type\":\"status\",\"files_done\":0,\"total_files\":1}'\nsleep 60 &\nwait\n"
			if err := os.WriteFile(resticPath, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			password := filepath.Join(root, "password")
			if err := os.WriteFile(password, []byte("offline-placeholder"), 0600); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "config.yml")
			data, _ := yaml.Marshal(map[string]any{"repository": filepath.Join(root, "unused-repository"), "target_path": root,
				"password_file": password, "state_path": filepath.Join(root, "state.json"), "restic_path": resticPath})
			if err := os.WriteFile(configPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			in, input := io.Pipe()
			defer in.Close()
			defer input.Close()
			out := &synchronizedOutput{progress: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan int, 1)
			go func() { done <- run(ctx, []string{configPath}, in, out) }()
			if _, err := io.WriteString(input, "machine run cleanup backup manual\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-out.progress:
			case <-time.After(5 * time.Second):
				t.Fatal("fake Restic task did not start")
			}
			wantCode := 0
			switch cause {
			case "signal":
				cancel()
				wantCode = 130
			case "stop":
				_, _ = io.WriteString(input, "stop\n")
			case "eof":
				_ = input.Close()
			}
			select {
			case code := <-done:
				if code != wantCode {
					t.Fatal("unexpected shutdown code:", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not cancel the process group")
			}
			journal, _ := os.ReadFile(filepath.Join(root, "state.json"))
			if !strings.Contains(string(journal), `"event":"interrupted"`) || !strings.Contains(string(journal), `"current":null`) {
				t.Fatal("shutdown did not finish persisted recovery state")
			}
		})
	}
}
