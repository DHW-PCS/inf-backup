"""Drive both inf-backup interfaces against a temporary real Restic repository."""

from __future__ import annotations

import argparse
import json
import queue
import subprocess
import threading
import time
from uuid import uuid4

PROTOCOL_PREFIX = "INF_BACKUP_EVENT "


def _docker(*args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["docker", *args], check=check, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT
    )


class Wrapper:
    def __init__(self, image: str, volume: str) -> None:
        self.process = subprocess.Popen(
            [
                "docker",
                "run",
                "--rm",
                "-i",
                "--entrypoint",
                "python3",
                "-v",
                f"{volume}:/work",
                image,
                "/app/app.py",
                "/work/config.yml",
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
        )
        assert self.process.stdin is not None
        assert self.process.stdout is not None
        self.input = self.process.stdin
        self.output = self.process.stdout
        self.queue: queue.Queue[str | None] = queue.Queue()
        self.lines: list[str] = []
        self.reader = threading.Thread(target=self._read_output, daemon=True)
        self.reader.start()
        self.read_until(lambda line: "Done (114.514s)" in line)

    def _read_output(self) -> None:
        for line in self.output:
            self.queue.put(line.rstrip())
        self.queue.put(None)

    def send(self, command: str) -> None:
        self.input.write(command + "\n")
        self.input.flush()

    def read_until(self, predicate, *, timeout: float = 60.0) -> str:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                line = self.queue.get(timeout=max(0.0, deadline - time.monotonic()))
            except queue.Empty:
                break
            if line is None:
                break
            self.lines.append(line)
            if predicate(line):
                return line
        raise RuntimeError(
            "wrapper output did not reach the expected state; recent output:\n"
            + "\n".join(self.lines[-20:])
        )

    def read_event(
        self, request_id: str, event_name: str, *, timeout: float = 60.0
    ) -> dict[str, object]:
        matched: dict[str, object] = {}

        def predicate(line: str) -> bool:
            if not line.startswith(PROTOCOL_PREFIX):
                return False
            payload = json.loads(line[len(PROTOCOL_PREFIX) :])
            if payload.get("request_id") == request_id and payload.get("event") == event_name:
                matched.update(payload)
                return True
            return False

        self.read_until(predicate, timeout=timeout)
        return matched

    def close(self) -> None:
        if self.process.poll() is None:
            self.send("stop")
        self.process.wait(timeout=15)
        self.reader.join(timeout=1)
        if self.process.returncode != 0:
            raise RuntimeError(f"wrapper exited with status {self.process.returncode}")


def run(image: str) -> None:
    volume = f"inf-backup-smoke-{uuid4().hex}"
    _docker("volume", "create", volume)
    try:
        setup_code = (
            "import os\n"
            "from pathlib import Path\n"
            "root=Path('/work')\n"
            "[(root/name).mkdir(exist_ok=True) for name in ('repo','target','tmp')]\n"
            "(root/'target'/'smoke.txt').write_text('inf-backup smoke test\\n')\n"
            "(root/'password').write_text('smoke-password\\n')\n"
            "(root/'config.yml').write_text("
            "'repository: /work/repo\\n"
            "target_path: /work/target\\n"
            "password_file: /work/password\\n"
            "tmp_dir: /work/tmp\\n"
            "state_path: /work/state.json\\n')\n"
            "os.chown(root,1000,1000)\n"
            "root.chmod(0o755)\n"
            "[os.chown(path,1000,1000) for path in root.rglob('*')]\n"
            "[path.chmod(0o755 if path.is_dir() else 0o644) for path in root.rglob('*')]\n"
        )
        _docker(
            "run",
            "--rm",
            "--user",
            "root",
            "--entrypoint",
            "python3",
            "-v",
            f"{volume}:/work",
            image,
            "-c",
            setup_code,
        )
        init = _docker(
            "run",
            "--rm",
            "--entrypoint",
            "restic",
            "-e",
            "RESTIC_REPOSITORY=/work/repo",
            "-e",
            "RESTIC_PASSWORD_FILE=/work/password",
            "-v",
            f"{volume}:/work",
            image,
            "init",
        )
        if "created restic repository" not in init.stdout:
            raise RuntimeError("temporary Restic repository was not initialized:\n" + init.stdout)

        human = Wrapper(image, volume)
        human.send("version")
        human.read_until(lambda line: line.startswith("Restic:"))
        human.send("ls")
        human.read_until(lambda line: line.strip() == "smoke.txt")
        human.send("halt")
        human.read_until(lambda line: "当前没有正在运行的任务" in line)
        human.send("backup human-smoke")
        human.read_until(lambda line: "[+] 备份完成并创建快照。" in line)
        human.send("snapshots")
        human.read_until(lambda line: line.startswith("[+] 共找到"))
        human.send("check")
        human.read_until(lambda line: "[+] 仓库检查完成" in line)
        human.close()
        if any(PROTOCOL_PREFIX in line for line in human.lines):
            raise RuntimeError("human commands leaked machine protocol frames")

        machine = Wrapper(image, volume)
        machine.send("machine protocol handshake-smoke")
        handshake = machine.read_event("handshake-smoke", "succeeded")
        if handshake.get("payload", {}).get("protocol_version") != 1:
            raise RuntimeError("machine protocol handshake did not report v1")
        for request_id, operation in (
            ("snapshots-smoke", "snapshots"),
            ("check-smoke", "check"),
            ("backup-smoke", "backup machine-smoke"),
        ):
            machine.send(f"machine run {request_id} {operation}")
            machine.read_event(request_id, "accepted")
            result = machine.read_event(request_id, "succeeded", timeout=120.0)
            if request_id == "backup-smoke" and not result.get("payload", {}).get("snapshot_id"):
                raise RuntimeError("machine backup success omitted snapshot_id")
        machine.close()
    finally:
        _docker("volume", "rm", "-f", volume, check=False)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("image", nargs="?", default="inf-backup-smoke:local")
    args = parser.parse_args()
    run(args.image)
    print("inf-backup container smoke test passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
