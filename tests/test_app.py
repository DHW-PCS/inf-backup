from __future__ import annotations

from contextlib import redirect_stdout
from io import StringIO
import json
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import call, patch

from app import app


class FakeProcess:
    def __init__(self, stdout: str, stderr: str = "", *, returncode: int = 0) -> None:
        self.stdout = StringIO(stdout)
        self.stderr = StringIO(stderr)
        self.returncode = returncode
        self.pid = 1234
        self.terminated = False

    def wait(self) -> int:
        return self.returncode

    def poll(self) -> int | None:
        return self.returncode if self.terminated else None

    def terminate(self) -> None:
        self.terminated = True


class AppTestCase(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        root = Path(self.temporary.name)
        self.password = root / "password"
        self.password.write_text("secret", encoding="utf-8")
        self.target = root / "target"
        self.target.mkdir()
        self.config = {
            "repository": str(root / "repository"),
            "target_path": str(self.target),
            "password_file": str(self.password),
            "restic_path": "restic",
            "state_path": str(root / "state.json"),
            "exclude_patterns": [],
        }

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def controller(self) -> app.TaskController:
        return app.TaskController(dict(self.config))

    @staticmethod
    def wait(controller: app.TaskController) -> None:
        with controller.lock:
            task = controller.active
        if task and task.thread:
            task.thread.join(timeout=2)

    def test_config_rejects_inline_password(self) -> None:
        config_path = Path(self.temporary.name) / "config.yml"
        config_path.write_text(
            "repository: /repo\n"
            "target_path: /target\n"
            "password_file: /password\n"
            "password: secret\n",
            encoding="utf-8",
        )
        with self.assertRaisesRegex(app.ConfigError, "内联"):
            app.load_config(str(config_path))

    def test_protocol_handshake_is_explicit_machine_output(self) -> None:
        controller = self.controller()
        output = StringIO()
        with (
            patch.object(app, "_get_restic_version", return_value="restic 0.18.1"),
            redirect_stdout(output),
        ):
            controller.protocol("handshake")
        line = output.getvalue().strip()
        self.assertTrue(line.startswith(app.PROTOCOL_PREFIX))
        payload = json.loads(line[len(app.PROTOCOL_PREFIX) :])
        self.assertEqual(payload["request_id"], "handshake")
        self.assertEqual(payload["payload"]["protocol_version"], 1)
        self.assertTrue(payload["payload"]["compatible"])

    def test_human_backup_is_friendly_and_never_emits_protocol_frames(self) -> None:
        controller = self.controller()
        lines: list[str] = []
        controller.print_human = lines.append
        controller.emit_machine = lambda record: self.fail(f"machine frame leaked: {record}")
        summary = {
            "message_type": "summary",
            "snapshot_id": "abcdef12",
            "total_files_processed": 3,
            "total_bytes_processed": 4096,
            "data_added": 1024,
        }
        with (
            patch.object(app, "_get_restic_version", return_value="restic 0.18.1"),
            patch.object(controller, "_execute", return_value=summary),
        ):
            controller.start("human", "human-request", "backup", ["manual"])
            self.wait(controller)

        rendered = "\n".join(lines)
        self.assertIn("开始备份", rendered)
        self.assertIn("备份完成并创建快照", rendered)
        self.assertIn("快照 ID: abcdef12", rendered)
        self.assertNotIn(app.PROTOCOL_PREFIX, rendered)
        self.assertNotIn('"message_type"', rendered)

    def test_machine_backup_emits_only_protocol_records(self) -> None:
        controller = self.controller()
        records: list[dict[str, object]] = []
        controller.emit_machine = records.append
        controller.print_human = lambda message: self.fail(f"human output leaked: {message}")
        summary = {"message_type": "summary", "snapshot_id": "abcdef12"}
        with (
            patch.object(app, "_get_restic_version", return_value="restic 0.18.1"),
            patch.object(controller, "_execute", return_value=summary),
        ):
            controller.start("machine", "request-1", "backup", ["manual"])
            self.wait(controller)

        self.assertEqual([record["event"] for record in records], ["accepted", "succeeded"])
        self.assertTrue(all(record["request_id"] == "request-1" for record in records))

    def test_human_and_machine_tasks_share_one_busy_guard(self) -> None:
        controller = self.controller()
        started = threading.Event()
        release = threading.Event()
        human_lines: list[str] = []
        machine_records: list[dict[str, object]] = []
        controller.print_human = human_lines.append
        controller.emit_machine = machine_records.append

        def blocked_execute(task, args):
            del task, args
            started.set()
            release.wait(timeout=2)
            return {"message_type": "summary", "snapshot_id": "abcdef12"}

        with (
            patch.object(app, "_get_restic_version", return_value="restic 0.18.1"),
            patch.object(controller, "_execute", side_effect=blocked_execute),
        ):
            controller.start("human", "human-request", "backup", [])
            self.assertTrue(started.wait(timeout=1))
            controller.start("machine", "machine-request", "snapshots", [])
            release.set()
            self.wait(controller)

        self.assertTrue(any(record["event"] == "busy" for record in machine_records))
        self.assertTrue(any("备份完成" in line for line in human_lines))

    def test_machine_task_blocks_human_task_with_human_message(self) -> None:
        controller = self.controller()
        started = threading.Event()
        release = threading.Event()
        human_lines: list[str] = []
        machine_records: list[dict[str, object]] = []
        controller.print_human = human_lines.append
        controller.emit_machine = machine_records.append

        def blocked_execute(task, args):
            del task, args
            started.set()
            release.wait(timeout=2)
            return {"snapshots": []}

        with (
            patch.object(app, "_get_restic_version", return_value="restic 0.18.1"),
            patch.object(controller, "_execute", side_effect=blocked_execute),
        ):
            controller.start("machine", "machine-request", "snapshots", [])
            self.assertTrue(started.wait(timeout=1))
            controller.start("human", "human-request", "check", [])
            release.set()
            self.wait(controller)

        self.assertTrue(any("上一个任务仍在运行" in line for line in human_lines))
        self.assertEqual([record["event"] for record in machine_records], ["accepted", "succeeded"])

    def test_restart_marks_persisted_active_request_interrupted(self) -> None:
        store = app.TaskStateStore(str(self.config["state_path"]))
        store.begin(app._event("request", "backup", "accepted", {}))
        restarted = app.TaskStateStore(str(self.config["state_path"]))
        record = restarted.find("request")
        self.assertIsNotNone(record)
        assert record is not None
        self.assertEqual(record["event"], "interrupted")
        self.assertTrue(record["payload"]["uncertain"])

    def test_backup_json_stream_becomes_progress_and_summary(self) -> None:
        controller = self.controller()
        task = app.ActiveTask("request", "backup", "machine")
        controller.active = task
        progress: list[dict[str, object]] = []
        controller._publish_progress = lambda active, payload: progress.append(payload)
        stdout = (
            '{"message_type":"status","percent_done":0.5,"files_done":2,'
            '"total_files":4,"bytes_done":1024,"total_bytes":2048,'
            '"current_files":["/target/world"]}\n'
            '{"message_type":"summary","snapshot_id":"abcdef12","data_added":10}\n'
        )
        fake = FakeProcess(stdout)
        with patch("app.app.subprocess.Popen", return_value=fake) as popen:
            summary = controller._execute(task, ["manual"])

        command = popen.call_args.args[0]
        environment = popen.call_args.kwargs["env"]
        self.assertEqual(command[:3], ["restic", "--json", "backup"])
        self.assertIn("--tag", command)
        self.assertNotIn(str(self.config["repository"]), command)
        self.assertEqual(environment["RESTIC_REPOSITORY"], self.config["repository"])
        self.assertEqual(summary["snapshot_id"], "abcdef12")
        self.assertEqual(progress[0]["current_file"], "/target/world")

    def test_snapshots_and_check_require_valid_structured_results(self) -> None:
        controller = self.controller()
        for operation, stdout, expected in (
            ("snapshots", '[{"id":"abcdef","time":"2026-07-29T00:00:00Z"}]\n', "snapshots"),
            (
                "check",
                '{"message_type":"summary","num_errors":0,"broken_packs":null}\n',
                "num_errors",
            ),
        ):
            with self.subTest(operation=operation):
                task = app.ActiveTask("request", operation, "machine")
                controller.active = task
                with patch("app.app.subprocess.Popen", return_value=FakeProcess(stdout)):
                    result = controller._execute(task, [])
                self.assertIn(expected, result)

        task = app.ActiveTask("request", "check", "machine")
        controller.active = task
        broken = (
            '{"message_type":"summary","num_errors":1,'
            '"broken_packs":["pack"],"suggest_repair_index":true}\n'
        )
        with patch("app.app.subprocess.Popen", return_value=FakeProcess(broken)):
            with self.assertRaisesRegex(app.ResticOperationError, "存在错误") as caught:
                controller._execute(task, [])
        self.assertEqual(caught.exception.kind, "repository_inconsistent")
        self.assertEqual(caught.exception.details["broken_packs"], ["pack"])

    def test_nonzero_exit_is_classified_without_raw_json(self) -> None:
        controller = self.controller()
        task = app.ActiveTask("request", "snapshots", "machine")
        controller.active = task
        stderr = '{"message_type":"exit_error","code":11,"message":"repository locked"}\n'
        with patch("app.app.subprocess.Popen", return_value=FakeProcess("", stderr, returncode=11)):
            with self.assertRaises(app.ResticOperationError) as caught:
                controller._execute(task, [])
        self.assertEqual(caught.exception.kind, "repository_locked")
        self.assertEqual(str(caught.exception), "repository locked")

    def test_halt_targets_the_active_process_group(self) -> None:
        controller = self.controller()
        process = FakeProcess("", returncode=0)
        process.returncode = 0
        process.terminated = False
        task = app.ActiveTask("request", "backup", "human", process=process)
        controller.active = task
        messages: list[str] = []
        controller.print_human = messages.append
        with patch("app.app.os.killpg") as kill_group:
            controller.halt()
        kill_group.assert_called_once_with(process.pid, app.signal.SIGTERM)
        self.assertTrue(task.cancel_requested)
        self.assertTrue(any("中止" in message for message in messages))

    def test_halt_force_kills_a_process_group_that_does_not_exit(self) -> None:
        class StuckThread:
            @staticmethod
            def join(timeout=None):
                del timeout

            @staticmethod
            def is_alive():
                return True

        controller = self.controller()
        process = FakeProcess("")
        task = app.ActiveTask("request", "backup", "human", thread=StuckThread(), process=process)
        controller.active = task
        controller.print_human = lambda message: None
        with (
            patch.object(app, "PROCESS_TERMINATION_GRACE_SECONDS", 0),
            patch("app.app.os.killpg") as kill_group,
        ):
            controller.halt()
        self.assertEqual(
            kill_group.call_args_list,
            [call(process.pid, app.signal.SIGTERM), call(process.pid, app.signal.SIGKILL)],
        )
