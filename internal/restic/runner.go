// Package restic owns process sessions and verifies Restic's JSON output.
package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"inf-backup/internal/config"
)

const TerminationGrace = 10 * time.Second
const maxOutput = 32 << 20

type Error struct {
	Message   string
	Kind      string
	ExitCode  *int
	Uncertain bool
	Details   map[string]any
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Payload() map[string]any {
	p := map[string]any{"message": e.Message, "kind": e.Kind, "uncertain": e.Uncertain}
	if e.ExitCode != nil {
		p["exit_code"] = *e.ExitCode
	}
	if e.Details != nil {
		p["details"] = e.Details
	}
	return p
}

type Runner interface {
	Version(context.Context) string
	Run(context.Context, string, []string, func(map[string]any)) (map[string]any, error)
}

type ProcessRunner struct {
	Config config.Config
	Grace  time.Duration
}

func New(cfg config.Config) *ProcessRunner {
	return &ProcessRunner{Config: cfg, Grace: TerminationGrace}
}

var versionRE = regexp.MustCompile(`(?i)\brestic\s+(\d+)\.(\d+)\.(\d+)\b`)

func Compatible(version string) bool {
	m := versionRE.FindStringSubmatch(version)
	if m == nil {
		return false
	}
	minimum := []int{0, 18, 1}
	for i, value := range m[1:] {
		n, err := strconv.Atoi(value)
		if err != nil {
			return false
		}
		if n != minimum[i] {
			return n > minimum[i]
		}
	}
	return true
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(remaining, n)])
	}
	return n, nil // Always drain the child's pipes, even after the limit.
}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, sig)
	}
}

// watchCancellation also kills descendants that outlive the session leader.
func watchCancellation(ctx context.Context, cmd *exec.Cmd, grace time.Duration, done <-chan struct{}) <-chan struct{} {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-done:
			if ctx.Err() != nil {
				signalGroup(cmd, syscall.SIGKILL)
			}
			return
		case <-ctx.Done():
			signalGroup(cmd, syscall.SIGTERM)
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-timer.C:
			signalGroup(cmd, syscall.SIGKILL)
		case <-done:
			// The parent may have exited while detached children still survive.
			signalGroup(cmd, syscall.SIGKILL)
		}
	}()
	return finished
}

func (r *ProcessRunner) Version(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.Command(r.Config.ResticPath, "version")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, stderr := &cappedBuffer{limit: 4096}, &cappedBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if cmd.Start() != nil {
		return "restic (unavailable)"
	}
	done := make(chan struct{})
	watch := watchCancellation(ctx, cmd, r.Grace, done)
	err := cmd.Wait()
	close(done)
	<-watch
	if err != nil {
		return "restic (unavailable)"
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.TrimSpace(line) != "" {
			return r.Config.Redact(strings.TrimSpace(line))
		}
	}
	return "restic (unknown)"
}

func (r *ProcessRunner) Arguments(operation string, tags []string) []string {
	op := operation
	if operation == "dry-run" {
		op = "backup"
	}
	args := []string{"--json", op}
	if operation == "dry-run" {
		args = append(args, "--dry-run")
	}
	if operation == "backup" || operation == "dry-run" {
		for _, tag := range tags {
			args = append(args, "--tag", tag)
		}
		for _, pattern := range r.Config.ExcludePatterns {
			args = append(args, "--exclude", pattern)
		}
		args = append(args, "--", r.Config.TargetPath)
	}
	return args
}

func invalid(message string) *Error { return &Error{Message: message, Kind: "invalid_output"} }

func (r *ProcessRunner) Run(ctx context.Context, operation string, tags []string, progress func(map[string]any)) (result map[string]any, resultErr error) {
	env, err := r.Config.Environment()
	if err != nil {
		return nil, &Error{Message: err.Error(), Kind: "configuration"}
	}
	if ctx.Err() != nil {
		return nil, &Error{Message: "operation interrupted by operator", Kind: "interrupted", Uncertain: true}
	}
	cmd := exec.Command(r.Config.ResticPath, r.Arguments(operation, tags)...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &Error{Message: "无法打开 Restic 输出管道", Kind: "configuration"}
	}
	defer stdout.Close()
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, &Error{Message: "无法打开 Restic 错误管道", Kind: "configuration"}
	}
	defer stderr.Close()
	if err = cmd.Start(); err != nil {
		return nil, &Error{Message: "无法启动 Restic；请检查 restic_path 和执行权限", Kind: "configuration"}
	}
	defer func() {
		var failure *Error
		if errors.As(resultErr, &failure) && operation == "backup" {
			failure.Uncertain = true
		}
	}()
	done := make(chan struct{})
	watch := watchCancellation(ctx, cmd, r.Grace, done)
	reaped := false
	defer func() {
		if !reaped {
			// A callback panic must not leave a child running outside the slot.
			signalGroup(cmd, syscall.SIGKILL)
			_ = stdout.Close()
			_ = stderr.Close()
			_ = cmd.Wait()
			close(done)
			<-watch
		}
	}()
	errOutput := &cappedBuffer{limit: 64 << 10}
	errDone := make(chan struct{})
	go func() {
		defer close(errDone)
		_, _ = io.Copy(errOutput, stderr)
	}()
	var parseErr error
	var summary map[string]any
	summaries := 0
	var snapshots bytes.Buffer
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxOutput)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if operation == "snapshots" {
			if snapshots.Len()+len(line)+1 > maxOutput {
				parseErr = invalid("快照列表超过 32 MiB 输出限制")
				break
			}
			snapshots.Write(line)
			snapshots.WriteByte('\n')
			continue
		}
		var payload map[string]any
		if json.Unmarshal(line, &payload) != nil || payload == nil {
			parseErr = invalid("Restic 输出必须是有效的 JSON 对象")
			break
		}
		if payload["message_type"] == "summary" {
			summary = payload
			summaries++
		}
		if (operation == "backup" || operation == "dry-run") && payload["message_type"] == "status" {
			p, err := progressPayload(payload)
			if err != nil {
				parseErr = err
				break
			}
			progress(p)
		}
	}
	if scanner.Err() != nil {
		parseErr = invalid("无法读取 Restic 输出或单行超过输出限制")
	}
	if parseErr != nil {
		signalGroup(cmd, syscall.SIGKILL)
		_, _ = io.Copy(io.Discard, stdout)
	}
	<-errDone
	waitErr := cmd.Wait()
	reaped = true
	close(done)
	<-watch
	if ctx.Err() != nil {
		return nil, &Error{Message: "operation interrupted by operator", Kind: "interrupted", Uncertain: true}
	}
	if parseErr != nil {
		return nil, parseErr
	}
	if waitErr != nil {
		code := cmd.ProcessState.ExitCode()
		kind := map[int]string{10: "repository_unavailable", 11: "repository_locked", 12: "authentication_failed"}[code]
		if kind == "" {
			kind = "restic_failed"
		}
		return nil, &Error{Message: r.Config.Redact(errorMessage(errOutput.String())), Kind: kind, ExitCode: &code}
	}
	if operation == "snapshots" {
		var items []map[string]any
		if json.Unmarshal(snapshots.Bytes(), &items) != nil || items == nil {
			return nil, invalid("restic snapshots 未返回快照对象数组")
		}
		for _, item := range items {
			if item == nil {
				return nil, invalid("restic snapshots 包含非对象记录")
			}
		}
		return map[string]any{"snapshots": items}, nil
	}
	if summaries != 1 {
		return nil, invalid(fmt.Sprintf("restic %s 未返回唯一 summary", operation))
	}
	switch operation {
	case "backup":
		id, ok := summary["snapshot_id"].(string)
		if !ok || id == "" || summary["dry_run"] == true {
			return nil, invalid("restic backup 成功输出缺少 snapshot_id 或标明 dry_run")
		}
	case "dry-run":
		id, exists := summary["snapshot_id"]
		if summary["dry_run"] != true || (exists && id != "") {
			return nil, invalid("restic dry-run summary 必须标明 dry_run 且不能包含快照 ID")
		}
	case "check":
		count, ok := summary["num_errors"].(float64)
		packs, exists := summary["broken_packs"]
		broken, isArray := packs.([]any)
		if !ok || count != 0 || (exists && packs != nil && (!isArray || len(broken) != 0)) || summary["suggest_repair_index"] == true {
			return nil, &Error{Message: "restic check 报告仓库存在错误或需要修复", Kind: "repository_inconsistent", Details: map[string]any{
				"num_errors": summary["num_errors"], "broken_packs": packs,
				"suggest_repair_index": summary["suggest_repair_index"], "suggest_prune": summary["suggest_prune"],
			}}
		}
	}
	return summary, nil
}

func progressPayload(payload map[string]any) (map[string]any, error) {
	p := map[string]any{}
	for _, key := range []string{"percent_done", "total_files", "files_done", "total_bytes", "bytes_done", "seconds_elapsed", "seconds_remaining", "error_count"} {
		value, exists := payload[key]
		if !exists {
			continue
		}
		n, ok := value.(float64)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || (key != "percent_done" && (math.Trunc(n) != n || n >= math.Exp2(63))) {
			return nil, invalid("Restic 进度包含无效数值")
		}
		p[key] = n
	}
	// Restic updates scanned totals independently of processed counts.
	// Totals can lag behind work, and percent_done can exceed 1.
	// Preserve these estimates; only exit status and summary establish success.
	if files, ok := payload["current_files"].([]any); ok && len(files) > 0 {
		if file, ok := files[0].(string); ok {
			p["current_file"] = file
		}
	}
	return p, nil
}

func errorMessage(stderr string) string {
	fallback := "restic 未返回错误详情"
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(line), &payload) == nil && payload != nil {
			if message, ok := payload["message"].(string); ok && message != "" {
				return message
			}
			if nested, ok := payload["error"].(map[string]any); ok {
				if message, ok := nested["message"].(string); ok && message != "" {
					return message
				}
			}
		} else if fallback == "restic 未返回错误详情" && !strings.HasPrefix(line, "{") && !strings.HasPrefix(line, "[") {
			fallback = line
		}
	}
	return fallback
}
