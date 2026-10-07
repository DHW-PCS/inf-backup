// Package console shares one task controller between human and machine commands.
package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"inf-backup/internal/config"
	"inf-backup/internal/protocol"
	"inf-backup/internal/restic"
	"inf-backup/internal/state"
)

const progressInterval = 3 * time.Second

type task struct {
	id, operation, mode string
	latest              protocol.Event
	cancel              context.CancelFunc
	done                chan struct{}
	progressKey         string
	lastProgress        time.Time
}

type Controller struct {
	cfg     config.Config
	runner  restic.Runner
	version string
	store   *state.Store
	out     io.Writer
	outMu   sync.Mutex
	mu      sync.Mutex
	active  *task
	closing bool
	now     func() time.Time
}

func New(ctx context.Context, cfg config.Config, out io.Writer, runner restic.Runner) (*Controller, error) {
	store, err := state.Open(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	if runner == nil {
		runner = restic.New(cfg)
	}
	return &Controller{cfg: cfg, out: out, runner: runner, store: store, version: runner.Version(ctx), now: time.Now}, nil
}

func (c *Controller) human(text string) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	fmt.Fprintln(c.out, c.cfg.Redact(text))
}

// Sanitize payload strings before both journal persistence and output framing.
func (c *Controller) sanitize(value any) any {
	switch v := value.(type) {
	case string:
		return c.cfg.Redact(v)
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			result[key] = c.sanitize(item)
		}
		return result
	case []map[string]any:
		result := make([]map[string]any, len(v))
		for i, item := range v {
			result[i] = c.sanitize(item).(map[string]any)
		}
		return result
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = c.sanitize(item)
		}
		return result
	case []string:
		result := make([]string, len(v))
		for i, item := range v {
			result[i] = c.cfg.Redact(item)
		}
		return result
	default:
		return value
	}
}

func (c *Controller) emit(record protocol.Event) {
	record.Payload = c.sanitize(record.Payload).(map[string]any)
	c.outMu.Lock()
	defer c.outMu.Unlock()
	fmt.Fprintln(c.out, record.Line())
}

func (c *Controller) Protocol(id string) {
	if id != "" && !protocol.ValidID(id) {
		c.inputError("machine", id, "protocol", "request_id 格式无效")
		return
	}
	c.emit(protocol.New(id, "protocol", "succeeded", map[string]any{
		"wrapper_version": protocol.WrapperVersion, "protocol_version": protocol.Version,
		"restic_version": c.version, "compatible": restic.Compatible(c.version),
		"progress_interval_seconds": progressInterval.Seconds(),
		"capabilities":              []string{"backup", "snapshots", "check", "status", "dry-run"},
	}))
}

func (c *Controller) inputError(mode, id, operation, message string) {
	if mode == "machine" {
		c.emit(protocol.New(id, operation, "failed", map[string]any{"kind": "invalid_request", "message": message, "uncertain": false}))
	} else {
		c.human(fmt.Sprintf("[!] 无法开始 %s: %s", operation, message))
	}
}

func (c *Controller) Start(mode, id, operation string, args []string) {
	if !protocol.ValidOperation(operation) || (mode != "machine" && operation == "dry-run") {
		c.inputError(mode, id, operation, "不支持的操作: "+operation)
		return
	}
	if mode == "machine" && !protocol.ValidID(id) {
		c.inputError(mode, id, operation, "request_id 格式无效")
		return
	}
	tags := []string{}
	if operation == "backup" || operation == "dry-run" {
		var err error
		tags, err = config.Tags(args)
		if err != nil {
			c.inputError(mode, id, operation, err.Error())
			return
		}
		tags = append(tags, time.Now().Format("20060102150405"))
	} else if len(args) != 0 {
		c.inputError(mode, id, operation, operation+" 不接受额外参数")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		c.inputError(mode, id, operation, "程序正在退出")
		return
	}
	if mode == "machine" {
		known := c.store.Find(id)
		if c.active != nil && c.active.id == id {
			known = &c.active.latest
		}
		if known != nil {
			if known.Operation != operation {
				c.inputError(mode, id, operation, "request_id 已用于其他操作")
			} else {
				c.emit(*known)
			}
			return
		}
	}
	if c.active != nil {
		if mode == "machine" {
			var activeID any
			if c.active.mode == "machine" {
				activeID = c.active.id
			}
			c.emit(protocol.New(id, operation, "busy", map[string]any{
				"message": "another task is already running", "active_request_id": activeID, "active_operation": c.active.operation,
			}))
		} else {
			c.human("[!] 上一个任务仍在运行。请等待完成，或输入 halt 中止当前操作。")
		}
		return
	}
	if !restic.Compatible(c.version) {
		c.inputError(mode, id, operation, "需要 restic >= 0.18.1，当前为 "+c.version)
		return
	}
	if _, err := c.cfg.Environment(); err != nil {
		c.inputError(mode, id, operation, err.Error())
		return
	}
	accepted := protocol.New(id, operation, "accepted", map[string]any{"message": "task accepted"})
	if err := c.store.Begin(accepted); err != nil {
		c.inputError(mode, id, operation, "无法写入任务状态文件；请检查目录权限")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &task{id: id, operation: operation, mode: mode, latest: accepted, cancel: cancel, done: make(chan struct{})}
	c.active = t
	if mode == "machine" {
		c.emit(accepted)
	}
	go c.run(ctx, t, tags)
}

func (c *Controller) run(ctx context.Context, t *task, tags []string) {
	defer t.cancel()
	var terminal protocol.Event
	// Even an internal panic must release the slot and record an uncertain result.
	defer func() {
		if recover() != nil {
			terminal = protocol.New(t.id, t.operation, "failed", map[string]any{
				"kind": "internal", "message": "任务内部错误；请检查快照后再决定是否重试", "uncertain": true,
			})
		}
		c.finish(t, terminal)
	}()
	if t.mode == "human" {
		c.renderStart(t.operation, tags)
	}
	payload, err := c.runner.Run(ctx, t.operation, tags, func(p map[string]any) {
		p = c.sanitize(p).(map[string]any)
		c.mu.Lock()
		defer c.mu.Unlock()
		t.latest = protocol.New(t.id, t.operation, "progress", p)
		// Retain every update for status recovery, but do not flood the panel's
		// console or block Restic's output pipe on every status record.
		now := c.now()
		if !t.lastProgress.IsZero() && now.Sub(t.lastProgress) < progressInterval {
			return
		}
		if t.mode == "machine" {
			c.emit(t.latest)
		} else if t.operation == "backup" {
			c.renderProgress(t, p)
		}
		t.lastProgress = now
	})
	if err == nil {
		terminal = protocol.New(t.id, t.operation, "succeeded", c.sanitize(payload).(map[string]any))
		return
	}
	var failure *restic.Error
	if !errors.As(err, &failure) {
		failure = &restic.Error{Message: "任务失败；请检查快照和配置", Kind: "internal", Uncertain: true}
	}
	event := "failed"
	if failure.Kind == "interrupted" {
		event = "interrupted"
	}
	terminal = protocol.New(t.id, t.operation, event, c.sanitize(failure.Payload()).(map[string]any))
}

func (c *Controller) finish(t *task, terminal protocol.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	persisted := c.store.Finish(terminal) == nil
	if !persisted {
		terminal.Payload["state_persisted"] = false
	}
	t.latest = terminal
	if t.mode == "machine" {
		c.emit(terminal)
	} else {
		if terminal.Event == "succeeded" {
			c.renderSuccess(t.operation, terminal.Payload)
		} else {
			c.renderError(t.operation, terminal.Payload)
		}
		if !persisted {
			c.human("[!] 任务已经结束，但状态日志写入失败；程序重启后可能无法查询本次结果。")
		}
	}
	c.active = nil
	close(t.done)
}

func (c *Controller) Status(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !protocol.ValidID(id) {
		c.emit(protocol.New(id, "status", "unknown", map[string]any{"message": "invalid request_id"}))
		return
	}
	if c.active != nil && c.active.id == id {
		c.emit(c.active.latest)
		return
	}
	if record := c.store.Find(id); record != nil {
		c.emit(*record)
		return
	}
	c.emit(protocol.New(id, "status", "unknown", map[string]any{"message": "request not found"}))
}

func (c *Controller) Wait() {
	c.mu.Lock()
	t := c.active
	c.mu.Unlock()
	if t != nil {
		<-t.done
	}
}

func (c *Controller) Halt() {
	c.mu.Lock()
	t := c.active
	if t != nil {
		t.cancel()
	}
	c.mu.Unlock()
	if t == nil {
		c.human("[?] 当前没有正在运行的任务。")
		return
	}
	c.human("[!] 正在中止当前操作...")
	select {
	case <-t.done:
		c.human("[-] 当前操作已停止。")
	case <-time.After(2 * restic.TerminationGrace):
		c.human("[!] 无法确认 Restic 已退出；请检查容器进程状态。")
	}
}

func (c *Controller) Shutdown() {
	c.mu.Lock()
	c.closing = true
	if c.active != nil {
		c.active.cancel()
	}
	c.mu.Unlock()
	c.Wait()
}

func (c *Controller) Banner() {
	c.human("=== Restic 备份控制台 v" + protocol.WrapperVersion + " ===")
	c.human("目标目录: " + c.cfg.TargetPath)
	c.human("支持命令: backup <tags>, check, snapshots, ls, version, halt, stop")
	c.human("维护协议: machine protocol（仅供自动化工具使用）")
	// Pterodactyl's existing ready matcher depends on this exact line.
	c.human(`[Server thread/INFO]: Done (114.514s)! For help, type "help"`)
	c.human("----------------------------")
}

// Handle returns true when the operator requests shutdown.
func (c *Controller) Handle(line string) bool {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false
	}
	command := strings.ToLower(parts[0])
	switch command {
	case "machine":
		c.machine(parts)
	case "halt":
		c.Halt()
	case "stop":
		c.Shutdown()
		c.human("程序已退出。")
		return true
	case "version":
		c.human("Wrapper:  v" + protocol.WrapperVersion)
		c.human("Restic:   " + c.version)
	case "ls":
		path := c.cfg.TargetPath
		if len(parts) > 1 {
			path = parts[1]
		}
		c.list(path)
	case "backup", "snapshots", "check":
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			c.human("[!] 无法创建任务标识")
			return false
		}
		c.Start("human", "human-"+hex.EncodeToString(id[:]), command, parts[1:])
	default:
		c.human("[?] 未知命令: " + command)
	}
	return false
}

func (c *Controller) machine(parts []string) {
	if len(parts) == 1 {
		c.inputError("machine", "", "protocol", "expected: machine protocol [request_id]")
		return
	}
	switch parts[1] {
	case "protocol":
		if len(parts) == 2 {
			c.Protocol("")
		} else if len(parts) == 3 {
			c.Protocol(parts[2])
		} else {
			c.inputError("machine", "", "protocol", "expected: machine protocol [request_id]")
		}
	case "status":
		if len(parts) == 3 {
			c.Status(parts[2])
		} else {
			c.inputError("machine", "", "status", "expected: machine status <request_id>")
		}
	case "run":
		if len(parts) >= 4 {
			c.Start("machine", parts[2], strings.ToLower(parts[3]), parts[4:])
		} else {
			c.inputError("machine", "", "unknown", "expected: machine run <request_id> <backup|snapshots|check|dry-run> [tags...]")
		}
	default:
		c.inputError("machine", "", "unknown", "unknown machine command: "+parts[1])
	}
}

func (c *Controller) list(path string) {
	entries, err := os.ReadDir(path)
	if err != nil {
		c.human("[!] 无法读取目录；请检查路径和权限")
		return
	}
	c.human("[*] " + path + " 的第一层内容:")
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		suffix := ""
		if entry.Type()&os.ModeSymlink != 0 {
			suffix = "@"
		} else if entry.IsDir() {
			suffix = "/"
		}
		c.human("    " + entry.Name() + suffix)
		count++
	}
	if count == 0 {
		c.human("    （目录为空）")
	}
}

// Kept here for consistent readable rendering of both fresh and replayed data.
func number(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	}
	return 0, false
}
