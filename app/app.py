"""Interactive and machine-readable restic controller for Pterodactyl.

Human commands are the default interface.  The ``machine`` namespace is a
separate, versioned interface used by inf_maintenance_tools.
"""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass
from datetime import datetime
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import threading
from typing import Any, Callable
from uuid import uuid4

import yaml

__version__ = "2.0.0"
PROTOCOL_VERSION = 1
PROTOCOL_PREFIX = "INF_BACKUP_EVENT "
MINIMUM_RESTIC_VERSION = (0, 18, 1)
PROCESS_TERMINATION_GRACE_SECONDS = 10.0
TERMINAL_EVENTS = frozenset({"succeeded", "failed", "interrupted"})
MACHINE_EVENTS = frozenset(
    {"accepted", "progress", "succeeded", "failed", "busy", "interrupted", "unknown"}
)
OPERATIONS = frozenset({"backup", "snapshots", "check"})
MACHINE_OPERATIONS = OPERATIONS | {"dry-run"}
BACKUP_OPERATIONS = frozenset({"backup", "dry-run"})
REQUEST_ID_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}\Z")
VERSION_RE = re.compile(r"\brestic\s+(\d+)\.(\d+)\.(\d+)\b", re.IGNORECASE)


class ConfigError(ValueError):
    """The wrapper configuration is unsafe or incomplete."""


class ResticOperationError(RuntimeError):
    """A restic process failed or returned an invalid response."""

    def __init__(
        self,
        message: str,
        *,
        kind: str = "restic_failed",
        exit_code: int | None = None,
        uncertain: bool = False,
        details: dict[str, object] | None = None,
    ) -> None:
        super().__init__(message)
        self.kind = kind
        self.exit_code = exit_code
        self.uncertain = uncertain
        self.details = dict(details or {})

    def as_payload(self) -> dict[str, object]:
        payload: dict[str, object] = {
            "kind": self.kind,
            "message": str(self),
            "uncertain": self.uncertain,
        }
        if self.exit_code is not None:
            payload["exit_code"] = self.exit_code
        if self.details:
            payload["details"] = self.details
        return payload


class OperationInterrupted(ResticOperationError):
    """The operator interrupted the active restic process."""

    def __init__(self, message: str = "operation interrupted by operator") -> None:
        super().__init__(message, kind="interrupted", uncertain=True)


@dataclass(slots=True)
class ActiveTask:
    request_id: str
    operation: str
    mode: str
    thread: threading.Thread | None = None
    process: subprocess.Popen[str] | None = None
    cancel_requested: bool = False
    latest_event: dict[str, object] | None = None


def _safe_print(message: str, *, lock: threading.Lock | None = None) -> None:
    if lock is None:
        print(message, flush=True)
        return
    with lock:
        print(message, flush=True)


def load_config(config_path: str = "/home/container/config.yml") -> dict[str, object]:
    """Load and validate the wrapper YAML configuration."""

    try:
        with open(config_path, "r", encoding="utf-8") as config_file:
            payload = yaml.safe_load(config_file)
    except FileNotFoundError as exc:
        raise ConfigError(f"找不到配置文件 {config_path}") from exc
    except yaml.YAMLError as exc:
        raise ConfigError(f"配置文件 YAML 无效: {exc}") from exc
    except OSError as exc:
        raise ConfigError(f"无法读取配置文件 {config_path}: {exc}") from exc

    if not isinstance(payload, dict):
        raise ConfigError("配置文件顶层必须是映射")
    if "password" in payload:
        raise ConfigError("不支持内联 password；请配置 password_file")

    config: dict[str, object] = dict(payload)
    for key in ("repository", "target_path", "password_file"):
        value = config.get(key)
        if not isinstance(value, str) or not value.strip():
            raise ConfigError(f"配置项 {key} 必须是非空字符串")
        config[key] = value.strip()

    for key, default in (
        ("restic_path", "restic"),
        ("state_path", "/home/container/.inf-backup-protocol.json"),
    ):
        value = config.get(key, default)
        if not isinstance(value, str) or not value.strip():
            raise ConfigError(f"配置项 {key} 必须是非空字符串")
        config[key] = value.strip()

    tmp_dir = config.get("tmp_dir")
    if tmp_dir is not None:
        if not isinstance(tmp_dir, str) or not tmp_dir.strip():
            raise ConfigError("配置项 tmp_dir 必须是非空字符串")
        config["tmp_dir"] = tmp_dir.strip()

    excludes = config.get("exclude_patterns", [])
    if not isinstance(excludes, list) or not all(
        isinstance(pattern, str) and pattern for pattern in excludes
    ):
        raise ConfigError("配置项 exclude_patterns 必须是非空字符串列表")
    config["exclude_patterns"] = list(excludes)
    return config


def _restic_environment(config: dict[str, object]) -> dict[str, str]:
    password_file = Path(str(config["password_file"]))
    if not password_file.is_file():
        raise ConfigError(f"password_file 不存在或不是文件: {password_file}")

    environment = os.environ.copy()
    environment["RESTIC_REPOSITORY"] = str(config["repository"])
    environment["RESTIC_PASSWORD_FILE"] = str(password_file)
    tmp_dir = config.get("tmp_dir")
    if tmp_dir:
        path = Path(str(tmp_dir))
        path.mkdir(parents=True, exist_ok=True)
        if not path.is_dir():
            raise ConfigError(f"tmp_dir 不是目录: {path}")
        environment["TMPDIR"] = str(path)
    return environment


def _get_restic_version(config: dict[str, object]) -> str:
    try:
        result = subprocess.run(
            [str(config["restic_path"]), "version"],
            capture_output=True,
            text=True,
            check=False,
            encoding="utf-8",
        )
    except OSError:
        return "restic (unavailable)"
    line = next((line.strip() for line in result.stdout.splitlines() if line.strip()), "")
    return line or "restic (unknown)"


def _restic_is_compatible(version: str) -> bool:
    match = VERSION_RE.search(version)
    return bool(match and tuple(int(part) for part in match.groups()) >= MINIMUM_RESTIC_VERSION)


def _normalise_tags(tags: list[str]) -> list[str]:
    normalised: list[str] = []
    for tag in tags:
        value = tag.strip()
        if not value or any(character.isspace() for character in value):
            raise ConfigError("备份标签不能为空或包含空白字符")
        if any(ord(character) < 32 or ord(character) == 127 for character in value):
            raise ConfigError("备份标签不能包含控制字符")
        normalised.append(value)
    return normalised


def _event(
    request_id: str | None,
    operation: str,
    event_name: str,
    payload: dict[str, object] | None = None,
) -> dict[str, object]:
    if event_name not in MACHINE_EVENTS:
        raise ValueError(f"unsupported machine event: {event_name}")
    return {
        "protocol": PROTOCOL_VERSION,
        "request_id": request_id,
        "operation": operation,
        "event": event_name,
        "payload": payload or {},
    }


class TaskStateStore:
    """Persist active and recent protocol requests without storing secrets."""

    def __init__(self, path: str, *, history_limit: int = 32) -> None:
        self.path = Path(path)
        self.history_limit = history_limit
        self.history: deque[dict[str, object]] = deque(maxlen=history_limit)
        self.current: dict[str, object] | None = None
        self._load()
        if self.current is not None:
            interrupted = _event(
                str(self.current.get("request_id") or ""),
                str(self.current.get("operation") or "unknown"),
                "interrupted",
                {
                    "kind": "wrapper_restarted",
                    "message": "wrapper restarted while the operation was active",
                    "uncertain": True,
                },
            )
            self.history.append(interrupted)
            self.current = None
            self.save()

    def _load(self) -> None:
        try:
            payload = json.loads(self.path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return
        except (OSError, json.JSONDecodeError):
            return
        if not isinstance(payload, dict):
            return
        history = payload.get("history", [])
        if isinstance(history, list):
            for record in history[-self.history_limit :]:
                if isinstance(record, dict):
                    self.history.append(record)
        current = payload.get("current")
        if isinstance(current, dict):
            self.current = current

    def save(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        temporary = self.path.with_name(f".{self.path.name}.{uuid4().hex}.tmp")
        data = {"current": self.current, "history": list(self.history)}
        try:
            temporary.write_text(
                json.dumps(data, ensure_ascii=False, separators=(",", ":")), encoding="utf-8"
            )
            temporary.chmod(0o600)
            os.replace(temporary, self.path)
        finally:
            try:
                temporary.unlink()
            except FileNotFoundError:
                pass

    def begin(self, record: dict[str, object]) -> None:
        self.current = record
        self.save()

    def finish(self, record: dict[str, object]) -> None:
        self.current = None
        self.history.append(record)
        self.save()

    def find(self, request_id: str) -> dict[str, object] | None:
        if self.current and self.current.get("request_id") == request_id:
            return self.current
        for record in reversed(self.history):
            if record.get("request_id") == request_id:
                return record
        return None


def _extract_error_message(stderr_lines: list[str]) -> str:
    fallback = ""
    for line in stderr_lines:
        stripped = line.strip()
        if not stripped:
            continue
        if not fallback:
            fallback = stripped
        try:
            payload = json.loads(stripped)
        except json.JSONDecodeError:
            continue
        if not isinstance(payload, dict):
            continue
        message = payload.get("message")
        if isinstance(message, str) and message.strip():
            return message.strip()
        nested = payload.get("error")
        if isinstance(nested, dict):
            nested_message = nested.get("message")
            if isinstance(nested_message, str) and nested_message.strip():
                return nested_message.strip()
    return fallback or "restic 未返回错误详情"


def _classify_exit_error(exit_code: int, stderr_lines: list[str]) -> ResticOperationError:
    message = _extract_error_message(stderr_lines)
    categories = {
        10: "repository_unavailable",
        11: "repository_locked",
        12: "authentication_failed",
    }
    return ResticOperationError(
        message, kind=categories.get(exit_code, "restic_failed"), exit_code=exit_code
    )


def _format_bytes(value: object) -> str:
    if not isinstance(value, (int, float)) or isinstance(value, bool) or value < 0:
        return "未知"
    amount = float(value)
    for unit in ("B", "KiB", "MiB", "GiB", "TiB"):
        if amount < 1024 or unit == "TiB":
            return f"{amount:.0f} {unit}" if unit == "B" else f"{amount:.1f} {unit}"
        amount /= 1024
    return "未知"


def _snapshot_label(snapshot: dict[str, object]) -> str:
    snapshot_id = snapshot.get("short_id") or snapshot.get("id") or "unknown"
    timestamp = snapshot.get("time") or "unknown time"
    hostname = snapshot.get("hostname") or "-"
    tags = snapshot.get("tags")
    tag_text = ", ".join(str(tag) for tag in tags) if isinstance(tags, list) and tags else "-"
    return f"{snapshot_id}  {timestamp}  主机: {hostname}  标签: {tag_text}"


class TaskController:
    """Run exactly one human or machine restic task at a time."""

    def __init__(self, config: dict[str, object]) -> None:
        self.config = config
        self.output_lock = threading.Lock()
        self.lock = threading.RLock()
        self.store = TaskStateStore(str(config["state_path"]))
        self.active: ActiveTask | None = None
        self._human_progress: dict[str, int] = {}

    def print_human(self, message: str) -> None:
        _safe_print(message, lock=self.output_lock)

    def emit_machine(self, record: dict[str, object]) -> None:
        _safe_print(
            PROTOCOL_PREFIX
            + json.dumps(record, ensure_ascii=False, separators=(",", ":"), sort_keys=True),
            lock=self.output_lock,
        )

    def protocol(self, request_id: str | None = None) -> None:
        if request_id is not None and not REQUEST_ID_RE.fullmatch(request_id):
            self.emit_machine(
                _event(
                    request_id,
                    "protocol",
                    "failed",
                    {"kind": "invalid_request", "message": "request_id 格式无效"},
                )
            )
            return
        restic_version = _get_restic_version(self.config)
        self.emit_machine(
            _event(
                request_id,
                "protocol",
                "succeeded",
                {
                    "wrapper_version": __version__,
                    "protocol_version": PROTOCOL_VERSION,
                    "restic_version": restic_version,
                    "compatible": _restic_is_compatible(restic_version),
                    "capabilities": ["backup", "snapshots", "check", "status", "dry-run"],
                },
            )
        )

    def _busy(self, mode: str, request_id: str, operation: str) -> None:
        with self.lock:
            active = self.active
            payload = {
                "message": "another task is already running",
                "active_request_id": (
                    active.request_id if active and active.mode == "machine" else None
                ),
                "active_operation": active.operation if active else None,
            }
        if mode == "machine":
            self.emit_machine(_event(request_id, operation, "busy", payload))
        else:
            self.print_human("[!] 上一个任务仍在运行。请等待完成，或输入 halt 中止当前操作。")

    def start(self, mode: str, request_id: str, operation: str, args: list[str]) -> None:
        with self.lock:
            if self.active is not None:
                self._busy(mode, request_id, operation)
                return
        allowed_operations = MACHINE_OPERATIONS if mode == "machine" else OPERATIONS
        if operation not in allowed_operations:
            self._input_error(mode, request_id, operation, f"不支持的操作: {operation}")
            return
        try:
            if mode == "machine" and not REQUEST_ID_RE.fullmatch(request_id):
                raise ConfigError("request_id 格式无效")
            safe_args = _normalise_tags(args) if operation in BACKUP_OPERATIONS else []
            if operation in BACKUP_OPERATIONS:
                safe_args.append(datetime.now().strftime("%Y%m%d%H%M%S"))
            if operation not in BACKUP_OPERATIONS and args:
                raise ConfigError(f"{operation} 不接受额外参数")
            restic_version = _get_restic_version(self.config)
            if not _restic_is_compatible(restic_version):
                raise ConfigError(
                    "需要 restic >= "
                    + ".".join(str(part) for part in MINIMUM_RESTIC_VERSION)
                    + f"，当前为 {restic_version}"
                )
            _restic_environment(self.config)
        except (ConfigError, OSError) as exc:
            self._input_error(mode, request_id, operation, str(exc))
            return

        with self.lock:
            if self.active is not None:
                self._busy(mode, request_id, operation)
                return
            accepted = _event(request_id, operation, "accepted", {"message": "task accepted"})
            task = ActiveTask(
                request_id=request_id, operation=operation, mode=mode, latest_event=accepted
            )
            self.active = task
            try:
                self.store.begin(accepted)
            except OSError as exc:
                self.active = None
                self._input_error(mode, request_id, operation, f"无法写入任务状态文件: {exc}")
                return
            thread = threading.Thread(
                target=self._run_task,
                args=(task, safe_args),
                name=f"inf-backup-{operation}",
                daemon=True,
            )
            task.thread = thread
            if mode == "machine":
                self.emit_machine(accepted)
            thread.start()

    def _input_error(self, mode: str, request_id: str, operation: str, message: str) -> None:
        if mode == "machine":
            self.emit_machine(
                _event(
                    request_id,
                    operation,
                    "failed",
                    {"kind": "invalid_request", "message": message, "uncertain": False},
                )
            )
        else:
            self.print_human(f"[!] 无法开始 {operation}: {message}")

    def _run_task(self, task: ActiveTask, args: list[str]) -> None:
        try:
            self._render_human_start(task, args)
            payload = self._execute(task, args)
            terminal = _event(task.request_id, task.operation, "succeeded", payload)
            state_persisted = self._finish(task, terminal)
            if task.mode == "machine":
                self.emit_machine(terminal)
            else:
                self._render_human_success(task.operation, payload)
                if not state_persisted:
                    self._render_state_warning()
        except OperationInterrupted as exc:
            terminal = _event(task.request_id, task.operation, "interrupted", exc.as_payload())
            state_persisted = self._finish(task, terminal)
            if task.mode == "machine":
                self.emit_machine(terminal)
            else:
                self.print_human("[!] 操作已中止；结果可能不确定，请在重试前检查仓库状态。")
                if not state_persisted:
                    self._render_state_warning()
        except (ConfigError, ResticOperationError, OSError) as exc:
            error = (
                exc
                if isinstance(exc, ResticOperationError)
                else ResticOperationError(str(exc), kind="configuration")
            )
            terminal = _event(task.request_id, task.operation, "failed", error.as_payload())
            state_persisted = self._finish(task, terminal)
            if task.mode == "machine":
                self.emit_machine(terminal)
            else:
                self._render_human_error(task.operation, error)
                if not state_persisted:
                    self._render_state_warning()
        except BaseException as exc:
            error = ResticOperationError(str(exc) or exc.__class__.__name__, kind="internal")
            terminal = _event(task.request_id, task.operation, "failed", error.as_payload())
            state_persisted = self._finish(task, terminal)
            if task.mode == "machine":
                self.emit_machine(terminal)
            else:
                self._render_human_error(task.operation, error)
                if not state_persisted:
                    self._render_state_warning()

    def _finish(self, task: ActiveTask, terminal: dict[str, object]) -> bool:
        state_persisted = True
        with self.lock:
            task.latest_event = terminal
            task.process = None
            try:
                self.store.finish(terminal)
            except OSError:
                state_persisted = False
                payload = terminal.get("payload")
                if isinstance(payload, dict):
                    payload["state_persisted"] = False
            finally:
                if self.active is task:
                    self.active = None
        return state_persisted

    def _render_state_warning(self) -> None:
        self.print_human(
            "[!] 任务已经结束，但状态日志写入失败；wrapper 重启后可能无法查询本次结果。"
        )

    def _render_human_start(self, task: ActiveTask, args: list[str]) -> None:
        if task.mode != "human":
            return
        if task.operation == "backup":
            self.print_human(f"[*] 开始备份: {self.config['target_path']}")
            self.print_human(f"[*] 标签: {', '.join(args)}")
        elif task.operation == "snapshots":
            self.print_human("[*] 正在读取仓库快照...")
        else:
            self.print_human("[*] 正在检查仓库一致性...")

    def _render_human_progress(self, payload: dict[str, object]) -> None:
        percent_value = payload.get("percent_done")
        percent = (
            max(0, min(100, int(float(percent_value) * 100)))
            if isinstance(percent_value, (int, float)) and not isinstance(percent_value, bool)
            else None
        )
        files_done = payload.get("files_done")
        total_files = payload.get("total_files")
        bytes_done = payload.get("bytes_done")
        total_bytes = payload.get("total_bytes")
        progress_key = str(percent) if percent is not None else f"{files_done}:{bytes_done}"
        if self._human_progress.get("backup") == hash(progress_key):
            return
        self._human_progress["backup"] = hash(progress_key)
        percentage = f"{percent}%" if percent is not None else "进行中"
        files = (
            f"{files_done}/{total_files} 个文件"
            if isinstance(files_done, int) and isinstance(total_files, int)
            else "文件数统计中"
        )
        sizes = f"{_format_bytes(bytes_done)}/{_format_bytes(total_bytes)}"
        current = payload.get("current_file")
        suffix = f" · 当前: {current}" if isinstance(current, str) and current else ""
        self.print_human(f"[~] {percentage} · {files} · {sizes}{suffix}")

    def _render_human_success(self, operation: str, payload: dict[str, object]) -> None:
        if operation == "backup":
            self.print_human("[+] 备份完成并创建快照。")
            self.print_human(f"    快照 ID: {payload.get('snapshot_id', '-')}")
            self.print_human(
                f"    已处理: {payload.get('total_files_processed', 0)} 个文件，"
                f"{_format_bytes(payload.get('total_bytes_processed'))}"
            )
            self.print_human(f"    新增数据: {_format_bytes(payload.get('data_added'))}")
            return
        if operation == "snapshots":
            snapshots = payload.get("snapshots", [])
            if not snapshots:
                self.print_human("[+] 仓库中没有快照。")
                return
            self.print_human(f"[+] 共找到 {len(snapshots)} 个快照:")
            for snapshot in snapshots:
                if isinstance(snapshot, dict):
                    self.print_human(f"    {_snapshot_label(snapshot)}")
                    paths = snapshot.get("paths")
                    if isinstance(paths, list) and paths:
                        self.print_human("      路径: " + ", ".join(str(path) for path in paths))
            return
        self.print_human("[+] 仓库检查完成，未发现一致性错误。")
        self.print_human(f"    错误数: {payload.get('num_errors', 0)}")
        broken_packs = payload.get("broken_packs")
        self.print_human(
            f"    损坏 pack: {len(broken_packs) if isinstance(broken_packs, list) else 0}"
        )
        if payload.get("suggest_repair_index"):
            self.print_human("    修复建议: 请先查看 restic check 输出，再评估 repair index。")
        if payload.get("suggest_prune"):
            self.print_human("    建议: 可运行 restic prune 回收未使用空间。")

    def _render_human_error(self, operation: str, error: ResticOperationError) -> None:
        labels = {
            "repository_locked": "仓库已被锁定",
            "repository_unavailable": "仓库不存在或无法访问",
            "authentication_failed": "仓库密码或认证失败",
            "configuration": "wrapper 配置错误",
            "invalid_output": "restic 返回了无法验证的输出",
        }
        label = labels.get(error.kind, f"restic {operation} 失败")
        self.print_human(f"[!] {label}: {error}")
        if error.kind == "repository_locked":
            self.print_human("[!] 请确认没有其他任务后，再人工运行 restic unlock。")
        elif error.kind == "repository_unavailable":
            self.print_human("[!] 请检查 repository 配置、网络和存储是否可用。")
        elif error.kind == "authentication_failed":
            self.print_human("[!] 请检查 password_file 路径、权限及文件内容。")
        elif error.kind == "configuration":
            self.print_human("[!] 请检查 config.yml 以及目标目录和临时目录权限。")
        elif error.kind == "invalid_output":
            self.print_human("[!] 请检查 Restic 版本和 wrapper 日志，不要依赖本次异常输出。")
        elif error.kind == "repository_inconsistent":
            details = error.details
            broken_packs = details.get("broken_packs")
            if isinstance(broken_packs, list):
                self.print_human(f"[!] 损坏 pack: {len(broken_packs)}")
            if details.get("suggest_repair_index"):
                self.print_human("[!] Restic 建议评估 repair index；修复前请先保留诊断信息。")
        if error.uncertain:
            self.print_human("[!] 结果可能不确定；请检查快照列表后再决定是否重试。")

    def _set_process(self, task: ActiveTask, process: subprocess.Popen[str]) -> None:
        with self.lock:
            if self.active is not task:
                raise OperationInterrupted()
            task.process = process
            if task.cancel_requested:
                self._terminate_process(process)

    @staticmethod
    def _terminate_process(process: subprocess.Popen[str]) -> None:
        if process.poll() is not None:
            return
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except (ProcessLookupError, PermissionError):
            try:
                process.terminate()
            except ProcessLookupError:
                pass

    @staticmethod
    def _kill_process(process: subprocess.Popen[str]) -> None:
        if process.poll() is not None:
            return
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            try:
                process.kill()
            except ProcessLookupError:
                pass

    def halt(self) -> None:
        with self.lock:
            task = self.active
            if task is None:
                self.print_human("[?] 当前没有正在运行的任务。")
                return
            task.cancel_requested = True
            process = task.process
        self.print_human("[!] 正在中止当前操作...")
        if process is not None:
            self._terminate_process(process)
        thread = task.thread
        if thread is not None and thread is not threading.current_thread():
            thread.join(timeout=PROCESS_TERMINATION_GRACE_SECONDS)
            if thread.is_alive():
                with self.lock:
                    process = task.process
                if process is not None:
                    self._kill_process(process)
                thread.join(timeout=PROCESS_TERMINATION_GRACE_SECONDS)
        if thread is not None and thread.is_alive():
            self.print_human("[!] 无法确认 Restic 已退出；请检查容器进程状态。")
        else:
            self.print_human("[-] 当前操作已停止。")

    def status(self, request_id: str) -> None:
        if not REQUEST_ID_RE.fullmatch(request_id):
            self.emit_machine(
                _event(request_id, "status", "unknown", {"message": "invalid request_id"})
            )
            return
        with self.lock:
            if self.active and self.active.request_id == request_id:
                record = self.active.latest_event
            else:
                record = self.store.find(request_id)
        if record is None:
            self.emit_machine(
                _event(request_id, "status", "unknown", {"message": "request not found"})
            )
            return
        self.emit_machine(dict(record))

    def _publish_progress(self, task: ActiveTask, payload: dict[str, object]) -> None:
        record = _event(task.request_id, task.operation, "progress", payload)
        with self.lock:
            task.latest_event = record
        if task.mode == "machine":
            self.emit_machine(record)
        elif task.operation == "backup":
            self._render_human_progress(payload)

    def _execute(self, task: ActiveTask, args: list[str]) -> dict[str, object]:
        if task.operation == "dry-run" and task.mode != "machine":
            raise ConfigError("dry-run 仅限 machine 接口")
        environment = _restic_environment(self.config)
        restic_operation = "backup" if task.operation == "dry-run" else task.operation
        command = [str(self.config["restic_path"]), "--json", restic_operation]
        if task.operation == "dry-run":
            command.append("--dry-run")
        if task.operation in BACKUP_OPERATIONS:
            for tag in args:
                command.extend(["--tag", tag])
            for pattern in self.config["exclude_patterns"]:
                command.extend(["--exclude", str(pattern)])
            command.append(str(self.config["target_path"]))

        process = subprocess.Popen(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
            bufsize=1,
            env=environment,
            shell=False,
            start_new_session=True,
        )
        self._set_process(task, process)
        assert process.stdout is not None
        assert process.stderr is not None

        stderr_lines: list[str] = []

        def read_stderr() -> None:
            for line in process.stderr:
                if len(stderr_lines) < 200:
                    stderr_lines.append(line.rstrip())

        stderr_reader = threading.Thread(target=read_stderr, daemon=True)
        stderr_reader.start()

        stdout_lines: list[str] = []
        parsed_lines: list[dict[str, object]] = []
        for line in process.stdout:
            stripped = line.strip()
            if not stripped:
                continue
            stdout_lines.append(stripped)
            if task.operation == "snapshots":
                continue
            try:
                payload = json.loads(stripped)
            except json.JSONDecodeError as exc:
                self._terminate_process(process)
                process.wait()
                stderr_reader.join(timeout=1)
                raise ResticOperationError(
                    f"restic {task.operation} 输出不是有效 JSON", kind="invalid_output"
                ) from exc
            if not isinstance(payload, dict):
                raise ResticOperationError(
                    f"restic {task.operation} 输出包含非对象记录", kind="invalid_output"
                )
            parsed_lines.append(payload)
            if task.operation in BACKUP_OPERATIONS and payload.get("message_type") == "status":
                current_files = payload.get("current_files")
                progress = {
                    key: payload[key]
                    for key in (
                        "percent_done",
                        "total_files",
                        "files_done",
                        "total_bytes",
                        "bytes_done",
                        "seconds_elapsed",
                        "seconds_remaining",
                        "error_count",
                    )
                    if key in payload
                }
                if isinstance(current_files, list) and current_files:
                    progress["current_file"] = str(current_files[0])
                self._publish_progress(task, progress)

        exit_code = process.wait()
        stderr_reader.join(timeout=2)
        with self.lock:
            cancelled = task.cancel_requested
            task.process = None
        if cancelled:
            raise OperationInterrupted()
        if exit_code != 0:
            raise _classify_exit_error(exit_code, stderr_lines)

        if task.operation == "snapshots":
            try:
                snapshots = json.loads("\n".join(stdout_lines))
            except json.JSONDecodeError as exc:
                raise ResticOperationError(
                    "restic snapshots 输出不是有效 JSON", kind="invalid_output"
                ) from exc
            if not isinstance(snapshots, list) or not all(
                isinstance(snapshot, dict) for snapshot in snapshots
            ):
                raise ResticOperationError("restic snapshots 未返回快照数组", kind="invalid_output")
            return {"snapshots": snapshots}

        summaries = [
            payload for payload in parsed_lines if payload.get("message_type") == "summary"
        ]
        if len(summaries) != 1:
            raise ResticOperationError(
                f"restic {task.operation} 未返回唯一 summary", kind="invalid_output"
            )
        summary = summaries[0]
        if task.operation == "dry-run":
            if summary.get("dry_run") is not True or summary.get("snapshot_id", "") != "":
                raise ResticOperationError(
                    "restic dry-run summary 必须标明 dry_run 且不能包含快照 ID",
                    kind="invalid_output",
                )
            return dict(summary)
        if task.operation == "backup":
            snapshot_id = summary.get("snapshot_id")
            if not isinstance(snapshot_id, str) or not snapshot_id:
                raise ResticOperationError(
                    "restic backup 成功输出缺少 snapshot_id", kind="invalid_output"
                )
            return dict(summary)

        num_errors = summary.get("num_errors")
        broken_packs = summary.get("broken_packs", [])
        suggest_repair_index = summary.get("suggest_repair_index", False)
        if (
            not isinstance(num_errors, int)
            or isinstance(num_errors, bool)
            or num_errors != 0
            or (broken_packs is not None and not isinstance(broken_packs, list))
            or bool(broken_packs)
            or suggest_repair_index is True
        ):
            raise ResticOperationError(
                "restic check 报告仓库存在错误或需要修复",
                kind="repository_inconsistent",
                details={
                    "num_errors": num_errors,
                    "broken_packs": broken_packs,
                    "suggest_repair_index": suggest_repair_index,
                    "suggest_prune": summary.get("suggest_prune"),
                },
            )
        return dict(summary)


def _handle_machine(controller: TaskController, parts: list[str]) -> None:
    if len(parts) == 1 or parts[1] == "protocol":
        if len(parts) not in {2, 3}:
            controller.emit_machine(
                _event(
                    None,
                    "protocol",
                    "failed",
                    {
                        "kind": "invalid_request",
                        "message": "expected: machine protocol [request_id]",
                    },
                )
            )
            return
        controller.protocol(parts[2] if len(parts) == 3 else None)
        return
    if parts[1] == "status":
        if len(parts) != 3:
            controller.emit_machine(
                _event(
                    None,
                    "status",
                    "failed",
                    {"kind": "invalid_request", "message": "expected: machine status <request_id>"},
                )
            )
            return
        controller.status(parts[2])
        return
    if parts[1] == "run":
        if len(parts) < 4:
            controller.emit_machine(
                _event(
                    None,
                    "unknown",
                    "failed",
                    {
                        "kind": "invalid_request",
                        "message": (
                            "expected: machine run <request_id> "
                            "<backup|snapshots|check|dry-run> [tags...]"
                        ),
                    },
                )
            )
            return
        controller.start("machine", parts[2], parts[3].lower(), parts[4:])
        return
    controller.emit_machine(
        _event(
            None,
            "unknown",
            "failed",
            {"kind": "invalid_request", "message": f"unknown machine command: {parts[1]}"},
        )
    )


def main(config_path: str = "/home/container/config.yml") -> int:
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(line_buffering=True)
    try:
        config = load_config(config_path)
        controller = TaskController(config)
    except (ConfigError, OSError) as exc:
        print(f"[!] 配置错误: {exc}", flush=True)
        return 1

    print(f"=== Restic 备份控制台 v{__version__} ===", flush=True)
    print(f"当前仓库: {config['repository']}", flush=True)
    print(f"目标目录: {config['target_path']}", flush=True)
    print("支持命令: backup <tags>, check, snapshots, ls, version, halt, stop", flush=True)
    print("维护协议: machine protocol（仅供自动化工具使用）", flush=True)
    # Pterodactyl matches this line to mark the wrapper as running.
    print('[Server thread/INFO]: Done (114.514s)! For help, type "help"', flush=True)
    print("----------------------------", flush=True)

    while True:
        try:
            line = sys.stdin.readline()
            if not line:
                break
            parts = line.strip().split()
            if not parts:
                continue
            command = parts[0].lower()
            if command == "machine":
                _handle_machine(controller, parts)
            elif command == "halt":
                controller.halt()
            elif command == "stop":
                with controller.lock:
                    running = controller.active is not None
                if running:
                    controller.halt()
                controller.print_human("程序已退出。")
                break
            elif command == "version":
                controller.print_human(f"Wrapper:  v{__version__}")
                controller.print_human(f"Restic:   {_get_restic_version(config)}")
            elif command == "ls":
                path = Path(parts[1] if len(parts) > 1 else str(config["target_path"]))
                if not path.is_dir():
                    controller.print_human(f"[!] 目录不存在或不是目录: {path}")
                    continue
                controller.print_human(f"[*] {path} 的第一层内容:")
                entries = []
                try:
                    for entry in os.scandir(path):
                        if entry.name.startswith("."):
                            continue
                        suffix = (
                            "/"
                            if entry.is_dir(follow_symlinks=False)
                            else "@" if entry.is_symlink() else ""
                        )
                        entries.append(entry.name + suffix)
                except OSError as exc:
                    controller.print_human(f"[!] 无法读取目录: {exc}")
                    continue
                if entries:
                    for entry in sorted(entries):
                        controller.print_human(f"    {entry}")
                else:
                    controller.print_human("    （目录为空）")
            elif command in OPERATIONS:
                controller.start("human", f"human-{uuid4().hex}", command, parts[1:])
            else:
                controller.print_human(f"[?] 未知命令: {command}")
        except KeyboardInterrupt:
            controller.halt()
            break
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1] if len(sys.argv) > 1 else "/home/container/config.yml"))
