package restic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"inf-backup/internal/config"
)

// TestResticHelper is a local subprocess fixture, never an actual Restic client.
func TestResticHelper(t *testing.T) {
	if os.Getenv("INF_BACKUP_TEST_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) == 1 && args[0] == "version" {
		fmt.Println("restic 0.18.1 compiled with local offline fixture")
		os.Exit(0)
	}
	if path := os.Getenv("INF_BACKUP_TEST_CAPTURE"); path != "" {
		data, _ := json.Marshal(map[string]any{
			"args": args, "repository": os.Getenv("RESTIC_REPOSITORY"),
			"password_file": os.Getenv("RESTIC_PASSWORD_FILE"), "tmp_dir": os.Getenv("TMPDIR"),
		})
		_ = os.WriteFile(path, data, 0600)
	}
	mode := os.Getenv("INF_BACKUP_TEST_MODE")
	if mode == "group" || mode == "leader" || mode == "child" || mode == "detached-child" {
		if mode != "leader" {
			signal.Ignore(syscall.SIGTERM)
		}
		if mode == "detached-child" {
			_ = os.Stdout.Close()
			_ = os.Stderr.Close()
		}
		if mode == "child" || mode == "detached-child" {
			_ = os.WriteFile(os.Getenv("INF_BACKUP_TEST_CHILD_READY"), []byte("ready"), 0600)
		}
		if mode == "group" || mode == "leader" {
			child := exec.Command(os.Args[0], "-test.run=^TestResticHelper$", "--", "child")
			childMode := "child"
			if mode == "leader" {
				childMode = "detached-child"
			}
			child.Env = append(os.Environ(), "INF_BACKUP_TEST_MODE="+childMode)
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if err := child.Start(); err != nil {
				os.Exit(2)
			}
			_ = os.WriteFile(os.Getenv("INF_BACKUP_TEST_PIDS"), []byte(fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)), 0600)
		}
		for {
			time.Sleep(time.Second)
		}
	}
	fmt.Fprint(os.Stdout, os.Getenv("INF_BACKUP_TEST_STDOUT"))
	fmt.Fprint(os.Stderr, os.Getenv("INF_BACKUP_TEST_STDERR"))
	code, _ := strconv.Atoi(os.Getenv("INF_BACKUP_TEST_EXIT"))
	os.Exit(code)
}

func fixture(t *testing.T) *ProcessRunner {
	t.Helper()
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, "restic-fixture")
	script := "#!/bin/sh\nexport GORACE=atexit_sleep_ms=0\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=^TestResticHelper$ -- \"$@\"\n"
	if err = os.WriteFile(launcher, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	password := filepath.Join(root, "password")
	if err = os.WriteFile(password, []byte("offline-placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INF_BACKUP_TEST_HELPER", "1")
	t.Setenv("INF_BACKUP_TEST_STDOUT", "")
	t.Setenv("INF_BACKUP_TEST_STDERR", "")
	t.Setenv("INF_BACKUP_TEST_EXIT", "0")
	t.Setenv("INF_BACKUP_TEST_MODE", "")
	return &ProcessRunner{Config: config.Config{
		Repository: filepath.Join(root, "unused-repository"), PasswordFile: password,
		ResticPath: launcher, TargetPath: filepath.Join(root, "unused-target"), TmpDir: filepath.Join(root, "tmp"),
		ExcludePatterns: []string{"cache/**"},
	}, Grace: 50 * time.Millisecond}
}

func TestVersionAndArgumentEnvironmentContract(t *testing.T) {
	r := fixture(t)
	if version := r.Version(context.Background()); !Compatible(version) {
		t.Fatal("fixture version not recognized:", version)
	}
	for _, version := range []string{"restic 0.18.0", "restic 0.17.9", "unavailable", "restic 0.18"} {
		if Compatible(version) {
			t.Fatal("incompatible version accepted:", version)
		}
	}
	for _, version := range []string{"restic 0.18.1", "restic 0.19.0", "restic 1.0.0"} {
		if !Compatible(version) {
			t.Fatal("compatible version rejected:", version)
		}
	}
	capture := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("INF_BACKUP_TEST_CAPTURE", capture)
	t.Setenv("INF_BACKUP_TEST_STDOUT", `{"message_type":"summary","dry_run":true}`+"\n")
	if _, err := r.Run(context.Background(), "dry-run", []string{"--dry-run", "20261008000000"}, func(map[string]any) {}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(capture)
	var actual struct {
		Args         []string `json:"args"`
		Repository   string   `json:"repository"`
		PasswordFile string   `json:"password_file"`
		TmpDir       string   `json:"tmp_dir"`
	}
	if err := json.Unmarshal(data, &actual); err != nil {
		t.Fatal(err)
	}
	want := []string{"--json", "backup", "--dry-run", "--tag", "--dry-run", "--tag", "20261008000000", "--exclude", "cache/**", "--", r.Config.TargetPath}
	if !reflect.DeepEqual(actual.Args, want) || actual.Repository != r.Config.Repository || actual.PasswordFile != r.Config.PasswordFile || actual.TmpDir != r.Config.TmpDir {
		t.Fatal("argument/environment contract changed:", actual.Args)
	}
	if reflect.DeepEqual(r.Arguments("backup", []string{"--dry-run"}), r.Arguments("dry-run", nil)) {
		t.Fatal("human tag turned into a Restic flag")
	}
	if args := r.Arguments("check", nil); !reflect.DeepEqual(args, []string{"--json", "check"}) {
		t.Fatal("check enabled data reads:", args)
	}
}

func TestStrictResticOutput(t *testing.T) {
	r := fixture(t)
	for _, tc := range []struct {
		name, op, stdout string
		valid            bool
	}{
		{"backup", "backup", `{"message_type":"summary","snapshot_id":"abc12345"}`, true},
		{"backup-missing-id", "backup", `{"message_type":"summary"}`, false},
		{"backup-dry-run", "backup", `{"message_type":"summary","snapshot_id":"abc","dry_run":true}`, false},
		{"empty", "backup", "", false},
		{"duplicate-summary", "backup", "{\"message_type\":\"summary\",\"snapshot_id\":\"abc\"}\n{\"message_type\":\"summary\",\"snapshot_id\":\"abc\"}\n", false},
		{"invalid-json", "backup", "not json", false},
		{"array", "backup", "[]", false},
		{"null", "backup", "null", false},
		{"dry-run", "dry-run", `{"message_type":"summary","dry_run":true}`, true},
		{"dry-run-empty-id", "dry-run", `{"message_type":"summary","dry_run":true,"snapshot_id":""}`, true},
		{"dry-run-missing", "dry-run", `{"message_type":"summary"}`, false},
		{"dry-run-number", "dry-run", `{"message_type":"summary","dry_run":1}`, false},
		{"dry-run-id", "dry-run", `{"message_type":"summary","dry_run":true,"snapshot_id":"abc"}`, false},
		{"dry-run-null-id", "dry-run", `{"message_type":"summary","dry_run":true,"snapshot_id":null}`, false},
		{"snapshots", "snapshots", "[\n{\"id\":\"abc\",\"time\":\"2026-10-08T00:00:00Z\"}\n]", true},
		{"no-snapshots", "snapshots", "[]", true},
		{"snapshots-object", "snapshots", "{}", false},
		{"snapshots-null", "snapshots", "null", false},
		{"snapshots-null-item", "snapshots", "[null]", false},
		{"check", "check", `{"message_type":"summary","num_errors":0,"broken_packs":null}`, true},
		{"check-missing-count", "check", `{"message_type":"summary"}`, false},
		{"check-boolean-count", "check", `{"message_type":"summary","num_errors":false}`, false},
		{"check-errors", "check", `{"message_type":"summary","num_errors":1}`, false},
		{"check-packs", "check", `{"message_type":"summary","num_errors":0,"broken_packs":["bad"]}`, false},
		{"check-repair", "check", `{"message_type":"summary","num_errors":0,"suggest_repair_index":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INF_BACKUP_TEST_STDOUT", tc.stdout+"\n")
			_, err := r.Run(context.Background(), tc.op, nil, func(map[string]any) {})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err != nil && tc.op == "backup" {
				var failure *Error
				if !errors.As(err, &failure) || !failure.Uncertain {
					t.Fatal("failed backup was not marked uncertain")
				}
			}
		})
	}
}

func TestProgressAndExitClassification(t *testing.T) {
	r := fixture(t)
	t.Setenv("INF_BACKUP_TEST_STDOUT", "{\"message_type\":\"status\",\"percent_done\":0.5,\"files_done\":2,\"total_files\":4,\"bytes_done\":1024,\"total_bytes\":2048,\"current_files\":[\"/offline/world\"]}\n{\"message_type\":\"summary\",\"snapshot_id\":\"abc\"}\n")
	var progress map[string]any
	if _, err := r.Run(context.Background(), "backup", nil, func(p map[string]any) { progress = p }); err != nil {
		t.Fatal(err)
	}
	if progress["files_done"] != float64(2) || progress["current_file"] != "/offline/world" {
		t.Fatal("progress not translated")
	}
	for _, code := range []int{1, 3, 10, 11, 12, 42} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Setenv("INF_BACKUP_TEST_EXIT", strconv.Itoa(code))
			t.Setenv("INF_BACKUP_TEST_STDERR", `{"message_type":"exit_error","message":"repository locked"}`+"\n")
			_, err := r.Run(context.Background(), "backup", nil, func(map[string]any) {})
			var failure *Error
			if !errors.As(err, &failure) || *failure.ExitCode != code || !failure.Uncertain || strings.Contains(err.Error(), "message_type") {
				t.Fatal("nonzero exit treated as success or leaked JSON:", err)
			}
			if code == 11 && failure.Kind != "repository_locked" {
				t.Fatal("lock error not classified")
			}
		})
	}
	for _, status := range []string{`{"files_done":-1}`, `{"files_done":true}`, `{"files_done":0.5}`, `{"total_bytes":-1}`, `{"percent_done":-0.1}`} {
		var payload map[string]any
		_ = json.Unmarshal([]byte(status), &payload)
		if _, err := progressPayload(payload); err == nil {
			t.Fatal("invalid progress accepted")
		}
	}
}

func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("subprocess fixture never started")
	return nil
}

func TestCancellationKillsCompleteProcessGroup(t *testing.T) {
	for _, mode := range []string{"group", "leader"} {
		t.Run(mode, func(t *testing.T) { testCancellationGroup(t, mode) })
	}
}

func testCancellationGroup(t *testing.T, mode string) {
	r := fixture(t)
	t.Setenv("INF_BACKUP_TEST_MODE", mode)
	pidPath := filepath.Join(t.TempDir(), "pids")
	t.Setenv("INF_BACKUP_TEST_PIDS", pidPath)
	childReady := filepath.Join(t.TempDir(), "child-ready")
	t.Setenv("INF_BACKUP_TEST_CHILD_READY", childReady)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, "backup", nil, func(map[string]any) {})
		done <- err
	}()
	data := waitForFile(t, pidPath)
	var parent, child int
	if _, err := fmt.Sscanf(string(data), "%d %d", &parent, &child); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-parent, syscall.SIGKILL) })
	waitForFile(t, childReady)
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		var failure *Error
		if !errors.As(err, &failure) || failure.Kind != "interrupted" || !failure.Uncertain {
			t.Fatal("cancellation not reported as interrupted:", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process group was not killed within grace period")
	}
	if mode == "group" && time.Since(started) < r.Grace {
		t.Fatal("fixture did not require SIGKILL escalation")
	}
	// A killed orphan can remain as a zombie briefly; it must have no live state.
	for _, pid := range []int{parent, child} {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
				break
			}
			status, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
			if strings.TrimSpace(string(status)) == "" || strings.HasPrefix(strings.TrimSpace(string(status)), "Z") {
				break
			}
			time.Sleep(10 * time.Millisecond)
			if time.Now().After(deadline) {
				t.Fatalf("process %d remains alive", pid)
			}
		}
	}
}

func TestBackupProgressMayOutrunScanner(t *testing.T) {
	for _, op := range []string{"backup", "dry-run"} {
		for _, ending := range []string{"success", "missing-summary", "missing-id", "nonzero-exit"} {
			t.Run(op+"/"+ending, func(t *testing.T) {
				r := fixture(t)
				statuses := []string{
					`{"message_type":"status","files_done":1,"bytes_done":1024,"percent_done":0}`,
					`{"message_type":"status","files_done":2,"total_files":0,"bytes_done":2048,"total_bytes":0,"percent_done":0}`,
					`{"message_type":"status","files_done":3,"total_files":2,"bytes_done":3072,"total_bytes":2048,"percent_done":1.5}`,
					`{"message_type":"status","files_done":4,"total_files":4,"bytes_done":4096,"total_bytes":4096,"percent_done":1}`,
				}
				output := strings.Join(statuses, "\n") + "\n"
				if ending != "missing-summary" {
					summary := `{"message_type":"summary","snapshot_id":"abc12345"}`
					if op == "dry-run" {
						summary = `{"message_type":"summary","dry_run":true}`
					}
					if ending == "missing-id" {
						summary = `{"message_type":"summary"}`
					}
					output += summary + "\n"
				}
				t.Setenv("INF_BACKUP_TEST_STDOUT", output)
				if ending == "nonzero-exit" {
					t.Setenv("INF_BACKUP_TEST_EXIT", "3")
				}
				var updates []map[string]any
				_, err := r.Run(context.Background(), op, nil, func(p map[string]any) { updates = append(updates, p) })
				if (err == nil) != (ending == "success") {
					t.Fatalf("completion validation changed: %v", err)
				}
				if len(updates) != 4 || updates[2]["files_done"] != float64(3) || updates[2]["total_files"] != float64(2) || updates[2]["bytes_done"] != float64(3072) || updates[2]["total_bytes"] != float64(2048) || updates[2]["percent_done"] != 1.5 {
					t.Fatalf("scanner estimates stopped work or changed counters: %+v", updates)
				}
				if err != nil && op == "backup" {
					var failure *Error
					if !errors.As(err, &failure) || !failure.Uncertain {
						t.Fatal("failed backup lost uncertainty:", err)
					}
				}
			})
		}
	}
}
