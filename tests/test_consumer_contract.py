from __future__ import annotations

from contextlib import redirect_stdout
from io import StringIO
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

from app import app

CONSUMER_ROOT = Path(__file__).resolve().parents[2] / "inf_maintenance_tools"


@unittest.skipUnless(CONSUMER_ROOT.is_dir(), "sibling inf_maintenance_tools is unavailable")
class ConsumerContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        sys.path.insert(0, str(CONSUMER_ROOT))
        try:
            from inf_maintenance.restic_backup import ResticProtocolEvent
        except ModuleNotFoundError as exc:
            raise unittest.SkipTest(f"consumer dependencies are unavailable: {exc.name}") from exc
        finally:
            sys.path.pop(0)
        cls.consumer_event = ResticProtocolEvent

    def test_existing_consumer_accepts_additive_dry_run_capability(self) -> None:
        output = StringIO()
        with (
            tempfile.TemporaryDirectory() as root,
            patch.object(app, "_get_restic_version", return_value="restic 0.18.1"),
            redirect_stdout(output),
        ):
            controller = app.TaskController(
                {"restic_path": "restic", "state_path": f"{root}/state"}
            )
            controller.protocol("contract-handshake")
        event = self.consumer_event.from_console_line(output.getvalue())
        self.assertIsNotNone(event)
        assert event is not None
        self.assertEqual(event.operation, "protocol")
        self.assertTrue(
            {"backup", "snapshots", "check", "status", "dry-run"}
            <= set(event.payload["capabilities"])
        )

    def test_existing_consumer_accepts_dry_run_frames_without_translation(self) -> None:
        for event_name, payload in (
            ("accepted", {"message": "task accepted"}),
            ("progress", {"files_done": 2, "bytes_done": 1024}),
            ("succeeded", {"message_type": "summary", "dry_run": True}),
            ("failed", {"kind": "invalid_output", "uncertain": False}),
            ("busy", {"active_operation": "backup"}),
            ("interrupted", {"kind": "wrapper_restarted", "uncertain": True}),
        ):
            with self.subTest(event=event_name):
                record = app._event("contract-preview", "dry-run", event_name, payload)
                parsed = self.consumer_event.from_console_line(
                    app.PROTOCOL_PREFIX + json.dumps(record)
                )
                self.assertIsNotNone(parsed)
                assert parsed is not None
                self.assertEqual(parsed.request_id, "contract-preview")
                self.assertEqual(parsed.operation, "dry-run")
                self.assertEqual(parsed.event, event_name)
                self.assertEqual(parsed.payload, payload)
