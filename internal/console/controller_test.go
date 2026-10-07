package console

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"inf-backup/internal/config"
	"inf-backup/internal/protocol"
	"inf-backup/internal/restic"
)

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type fakeRunner struct {
	mu      sync.Mutex
	version string
	calls   int
	tags    []string
	run     func(context.Context, string, []string, func(map[string]any)) (map[string]any, error)
}

func (f *fakeRunner) Version(context.Context) string {
	if f.version == "" {
		return "restic 0.18.1"
	}
	return f.version
}
func (f *fakeRunner) Run(ctx context.Context, op string, tags []string, progress func(map[string]any)) (map[string]any, error) {
	f.mu.Lock()
	f.calls++
	f.tags = append([]string(nil), tags...)
	f.mu.Unlock()
	if f.run != nil {
		return f.run(ctx, op, tags, progress)
	}
	switch op {
	case "dry-run":
		return map[string]any{"message_type": "summary", "dry_run": true}, nil
	case "snapshots":
		return map[string]any{"snapshots": []map[string]any{{"id": "abc123456789", "time": "2026-10-08T00:00:00Z", "hostname": "offline", "tags": []string{"manual"}, "paths": []string{"/offline/world"}}}}, nil
	case "check":
		return map[string]any{"message_type": "summary", "num_errors": 0}, nil
	default:
		progress(map[string]any{"percent_done": 0.5, "files_done": float64(2), "total_files": float64(4), "bytes_done": float64(1024), "total_bytes": float64(2048)})
		return map[string]any{"message_type": "summary", "snapshot_id": "abc12345", "total_files_processed": 4, "total_bytes_processed": 2048, "data_added": 512}, nil
	}
}
func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func controller(t *testing.T, f *fakeRunner) (*Controller, *safeBuffer) {
	t.Helper()
	root := t.TempDir()
	password := filepath.Join(root, "password")
	if err := os.WriteFile(password, []byte("offline-placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Repository: "rclone:offline:repository", PasswordFile: password, ResticPath: "unused-restic",
		StatePath: filepath.Join(root, "journal", "state.json"), TargetPath: filepath.Join(root, "target")}
	out := &safeBuffer{}
	c, err := New(context.Background(), cfg, out, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Shutdown)
	return c, out
}

func events(t *testing.T, out *safeBuffer) []protocol.Event {
	t.Helper()
	result := []protocol.Event{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !strings.HasPrefix(line, protocol.Prefix) {
			continue
		}
		var event protocol.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, protocol.Prefix)), &event); err != nil {
			t.Fatal("invalid frame:", err)
		}
		result = append(result, event)
	}
	return result
}

func TestHumanInterfacesRemainReadable(t *testing.T) {
	f := &fakeRunner{}
	c, out := controller(t, f)
	c.Banner()
	for _, command := range []string{"backup --dry-run 中文", "snapshots", "check", "version", "dry-run", "halt"} {
		c.Handle(command)
		c.Wait()
	}
	text := out.String()
	for _, expected := range []string{"v3.0.1", "Done (114.514s)!", "开始备份", "备份完成并创建快照", "2/4 个文件", "1.0 KiB/2.0 KiB", "abc12345", "共找到 1 个快照", "主机: offline", "仓库检查完成", "未知命令: dry-run"} {
		if !strings.Contains(text, expected) {
			t.Fatal("human output missing:", expected)
		}
	}
	for _, forbidden := range []string{protocol.Prefix, "request_id", "human-", "message_type", c.cfg.Repository} {
		if strings.Contains(text, forbidden) {
			t.Fatal("human console leaked machine data or repository:", forbidden)
		}
	}
	if f.count() != 3 {
		t.Fatal("human dry-run executed a task")
	}
}

func TestMachineCorrelationReplayAndNoDuplicateExecution(t *testing.T) {
	f := &fakeRunner{}
	c, out := controller(t, f)
	c.Handle("machine protocol handshake")
	c.Handle("machine run request-1 backup manual")
	c.Wait()
	c.Handle("machine status request-1")
	c.Handle("machine run request-1 backup different-tag")
	c.Handle("machine run request-1 check")
	c.Handle("machine status absent")
	e := events(t, out)
	if e[0].ID() != "handshake" || e[0].Payload["wrapper_version"] != "3.0.1" || e[0].Payload["compatible"] != true || e[0].Payload["progress_interval_seconds"] != float64(3) {
		t.Fatal("incorrect handshake")
	}
	if f.count() != 1 || e[1].Event != "accepted" || e[2].Event != "progress" || e[3].Event != "succeeded" || !reflect.DeepEqual(e[3], e[4]) || !reflect.DeepEqual(e[3], e[5]) || e[6].Event != "failed" || e[7].Event != "unknown" {
		t.Fatal("duplicate request or status replay executed work:", e)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !strings.HasPrefix(line, protocol.Prefix) {
			t.Fatal("human output leaked into machine task")
		}
	}
	f.mu.Lock()
	tags := append([]string(nil), f.tags...)
	f.mu.Unlock()
	if len(tags) != 2 || tags[0] != "manual" || !regexp.MustCompile(`^\d{14}$`).MatchString(tags[1]) {
		t.Fatal("automatic timestamp tag lost")
	}
	restarted, err := New(context.Background(), c.cfg, out, f)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Status("request-1")
	if f.count() != 1 {
		t.Fatal("restart status retried task")
	}
}

func TestProgressOutputIsBoundedWithFreshStatusAndImmediateTerminal(t *testing.T) {
	for _, tc := range []struct{ mode, operation, terminal string }{
		{"machine", "backup", "succeeded"},
		{"machine", "dry-run", "succeeded"},
		{"machine", "backup", "failed"},
		{"machine", "backup", "interrupted"},
		{"human", "backup", "succeeded"},
		{"human", "backup", "failed"},
		{"human", "backup", "interrupted"},
	} {
		t.Run(tc.mode+"-"+tc.operation+"-"+tc.terminal, func(t *testing.T) {
			f := &fakeRunner{}
			c, out := controller(t, f)
			base := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
			now := base
			c.now = func() time.Time { return now }
			f.run = func(ctx context.Context, op string, tags []string, report func(map[string]any)) (map[string]any, error) {
				// Simulate 602 updates over six seconds without sleeping. Only the
				// first, 3-second and 6-second updates should reach the console.
				for i := 0; i <= 601; i++ {
					now = base.Add(time.Duration(i) * 10 * time.Millisecond)
					report(map[string]any{"files_done": float64(i), "total_files": float64(1000), "bytes_done": float64(i * 1024), "total_bytes": float64(1024000)})
					if i == 299 && tc.mode == "machine" {
						c.Status("request")
					}
				}
				if tc.terminal != "succeeded" {
					return nil, &restic.Error{Message: "offline fixture failure", Kind: tc.terminal, Uncertain: true}
				}
				return map[string]any{"snapshot_id": "abc12345", "dry_run": op == "dry-run"}, nil
			}
			c.Start(tc.mode, "request", tc.operation, nil)
			c.Wait()
			if tc.mode == "machine" {
				e := events(t, out)
				if len(e) != 6 || e[0].Event != "accepted" || e[5].Event != tc.terminal {
					t.Fatal("progress flood or delayed terminal:", e)
				}
				for i, count := range []float64{0, 299, 300, 600} {
					if e[i+1].Event != "progress" || e[i+1].Payload["files_done"] != count {
						t.Fatal("rate limit or latest status lost:", e)
					}
				}
				c.Status("request")
				e = events(t, out)
				if !reflect.DeepEqual(e[5], e[6]) {
					t.Fatal("terminal replay changed")
				}
			} else {
				text := out.String()
				if strings.Count(text, "[~]") != 3 || !strings.Contains(text, "300/1000 个文件") || !strings.Contains(text, "600/1000 个文件") || strings.Contains(text, protocol.Prefix) {
					t.Fatal("human progress flood or unreadable progress:", text)
				}
				if tc.terminal == "succeeded" && !strings.Contains(text, "备份完成") || tc.terminal != "succeeded" && !strings.Contains(text, "offline fixture failure") {
					t.Fatal("human terminal missing:", text)
				}
			}
		})
	}
}

func TestInvalidRequestsDoNotExecute(t *testing.T) {
	for _, tc := range []struct {
		mode, id, op, version string
		tags                  []string
	}{
		{"machine", "bad/id", "backup", "", nil},
		{"machine", "request", "dry-run", "", []string{"bad tag"}},
		{"machine", "request", "backup", "", []string{"bad\x00tag"}},
		{"machine", "request", "check", "", []string{"--read-data"}},
		{"machine", "request", "backup", "restic 0.18.0", nil},
		{"machine", "request", "unsupported", "", nil},
		{"human", "request", "dry-run", "", nil},
	} {
		t.Run(tc.mode+tc.id+tc.op+tc.version, func(t *testing.T) {
			f := &fakeRunner{version: tc.version}
			c, out := controller(t, f)
			c.Start(tc.mode, tc.id, tc.op, tc.tags)
			c.Wait()
			if f.count() != 0 {
				t.Fatal("invalid request reached Restic")
			}
			if tc.mode == "machine" {
				e := events(t, out)
				if len(e) != 1 || e[0].Event != "failed" || e[0].Payload["uncertain"] != false {
					t.Fatal("invalid request was not rejected deterministically")
				}
			} else if strings.Contains(out.String(), protocol.Prefix) {
				t.Fatal("human invalid request emitted a frame")
			}
		})
	}
}

func TestDryRunSharesSlotWithHumanAndMachine(t *testing.T) {
	for _, tc := range []struct{ firstMode, firstOp, secondMode, secondOp string }{
		{"human", "backup", "machine", "dry-run"},
		{"machine", "dry-run", "human", "backup"},
		{"machine", "dry-run", "machine", "check"},
	} {
		t.Run(tc.firstMode+tc.firstOp+tc.secondMode+tc.secondOp, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f := &fakeRunner{run: func(ctx context.Context, op string, tags []string, progress func(map[string]any)) (map[string]any, error) {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
				}
				return map[string]any{"message_type": "summary", "dry_run": op == "dry-run", "snapshot_id": "abc"}, nil
			}}
			c, out := controller(t, f)
			defer once.Do(func() { close(release) })
			c.Start(tc.firstMode, "active", tc.firstOp, nil)
			<-started
			c.Start(tc.secondMode, "other", tc.secondOp, nil)
			if tc.firstMode == "machine" {
				c.Start("machine", "active", tc.firstOp, nil)
			}
			once.Do(func() { close(release) })
			c.Wait()
			if f.count() != 1 {
				t.Fatal("slot allowed parallel execution or duplicate replay")
			}
			if tc.secondMode == "machine" {
				found := false
				for _, event := range events(t, out) {
					found = found || (event.ID() == "other" && event.Event == "busy" && event.Payload["active_operation"] == tc.firstOp)
				}
				if !found {
					t.Fatal("missing busy frame")
				}
			} else if !strings.Contains(out.String(), "上一个任务仍在运行") {
				t.Fatal("missing human busy message")
			}
		})
	}
}

func TestConcurrentStartsOnlyRunOneTask(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	f := &fakeRunner{run: func(ctx context.Context, op string, tags []string, progress func(map[string]any)) (map[string]any, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return map[string]any{"num_errors": 0}, nil
	}}
	c, _ := controller(t, f)
	var starts sync.WaitGroup
	for i := 0; i < 30; i++ {
		starts.Add(1)
		go func(i int) {
			defer starts.Done()
			c.Start("machine", "request-"+strings.Repeat("x", i), "check", nil)
		}(i)
	}
	starts.Wait()
	<-started
	close(release)
	c.Wait()
	if f.count() != 1 {
		t.Fatal("multiple concurrent tasks ran")
	}
}

func TestCancellationPersistsAndShutdownRejectsNewWork(t *testing.T) {
	for _, operation := range []string{"backup", "dry-run"} {
		t.Run(operation, func(t *testing.T) {
			started := make(chan struct{})
			f := &fakeRunner{run: func(ctx context.Context, op string, tags []string, progress func(map[string]any)) (map[string]any, error) {
				close(started)
				<-ctx.Done()
				return nil, &restic.Error{Message: "operation interrupted", Kind: "interrupted", Uncertain: true}
			}}
			c, out := controller(t, f)
			c.Start("machine", "request", operation, nil)
			<-started
			c.Halt()
			c.Status("request")
			e := events(t, out)
			if len(e) != 3 || e[1].Event != "interrupted" || !reflect.DeepEqual(e[1], e[2]) || e[1].Payload["uncertain"] != true {
				t.Fatal("interruption not persisted/replayed")
			}
			c.Shutdown()
			c.Start("machine", "new-request", "backup", nil)
			if f.count() != 1 {
				t.Fatal("shutdown accepted a new task")
			}
		})
	}
}

func TestPersistenceFailureKeepsVerifiedSuccess(t *testing.T) {
	f := &fakeRunner{}
	c, out := controller(t, f)
	f.run = func(context.Context, string, []string, func(map[string]any)) (map[string]any, error) {
		dir := filepath.Dir(c.cfg.StatePath)
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dir, []byte("block state writes"), 0600); err != nil {
			return nil, err
		}
		return map[string]any{"snapshot_id": "abc12345"}, nil
	}
	c.Start("machine", "request", "backup", nil)
	c.Wait()
	c.Status("request")
	e := events(t, out)
	if e[1].Event != "succeeded" || e[1].Payload["state_persisted"] != false || !reflect.DeepEqual(e[1], e[2]) {
		t.Fatal("state-write failure changed a verified backup or lost replay")
	}
}

func TestMachineDryRunTerminalCanBeReplayedAfterRestart(t *testing.T) {
	f := &fakeRunner{}
	c, out := controller(t, f)
	c.Handle("machine run preview dry-run manual")
	c.Wait()
	reopened, err := New(context.Background(), c.cfg, out, f)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Handle("machine status preview")
	e := events(t, out)
	if len(e) != 3 || e[1].Operation != "dry-run" || e[1].Payload["dry_run"] != true || !reflect.DeepEqual(e[1], e[2]) || f.count() != 1 {
		t.Fatal("dry-run terminal contract or replay changed")
	}
}

func TestErrorsAndJournalRedactRepositoryCredentials(t *testing.T) {
	f := &fakeRunner{}
	c, out := controller(t, f)
	f.run = func(context.Context, string, []string, func(map[string]any)) (map[string]any, error) {
		return nil, &restic.Error{Message: "failure for " + c.cfg.Repository + " https://user:placeholder@example.invalid", Kind: "repository_locked"}
	}
	c.Start("machine", "request", "backup", nil)
	c.Wait()
	data, err := os.ReadFile(c.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{out.String(), string(data)} {
		if strings.Contains(text, c.cfg.Repository) || strings.Contains(text, "placeholder") {
			t.Fatal("credentials reached output or journal")
		}
	}
}
