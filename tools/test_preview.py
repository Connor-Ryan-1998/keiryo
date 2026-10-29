"""Preview isolation, Docker endpoint selection, and real-process lifecycle checks."""

import json
import os
from pathlib import Path
import queue
import signal
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import preview


class PreviewTests(unittest.TestCase):
    def test_configs_are_private_loopback_and_keep_collection_defaults(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            hub_path, agent_path = preview.write_configs(directory, 19080, 19081, "/local/docker.sock")
            hub = json.loads(hub_path.read_text())
            agent = json.loads(agent_path.read_text())
            self.assertEqual(hub["ui_listen"], "127.0.0.1:19080")
            self.assertEqual(hub["agent_listen"], "127.0.0.1:19081")
            self.assertEqual(hub["auth_mode"], "local")
            self.assertEqual(hub["public_url"], "http://127.0.0.1:19080")
            self.assertEqual(agent["hub_url"], "http://127.0.0.1:19081")
            self.assertNotIn("interval_seconds", agent)
            self.assertEqual(agent["docker_socket"], "/local/docker.sock")
            self.assertIn("devfs", agent["exclude_filesystems"])
            credential = Path(agent["credential_file"])
            self.assertEqual(hub["agents"][agent["host_id"]], str(credential))
            self.assertEqual(credential.stat().st_mode & 0o777, 0o600)
            self.assertGreaterEqual(len(credential.read_text()), 32)
            self.assertEqual(Path(hub["database"]).parent, directory)
            self.assertEqual(Path(agent["queue_database"]), directory / "agent.redb")

    def test_docker_host_override_and_context_endpoint(self):
        with patch.dict(os.environ, {"DOCKER_HOST": "unix:///custom/docker.sock"}, clear=True):
            self.assertEqual(preview.docker_socket(), "/custom/docker.sock")
        endpoint = subprocess.CompletedProcess([], 0, "unix:///Users/local/.docker/run/docker.sock\n", "")
        with patch.dict(os.environ, {}, clear=True), patch("preview.subprocess.run", return_value=endpoint):
            self.assertEqual(preview.docker_socket(), "/Users/local/.docker/run/docker.sock")
        # Docker's explicit context takes precedence over DOCKER_HOST.
        with patch.dict(os.environ, {"DOCKER_HOST":"tcp://remote:2375", "DOCKER_CONTEXT":"desktop-linux"}, clear=True), patch("preview.subprocess.run", return_value=endpoint):
            self.assertEqual(preview.docker_socket(), "/Users/local/.docker/run/docker.sock")

    def test_remote_docker_endpoints_are_not_contacted(self):
        for endpoint in ["ssh://production", "tcp://production:2376", "unix://remote/socket", "unix:relative.sock"]:
            with self.subTest(endpoint=endpoint), patch.dict(os.environ, {"DOCKER_HOST":endpoint}, clear=True):
                with self.assertRaises(ValueError):
                    preview.docker_socket()
        endpoint = subprocess.CompletedProcess([], 0, "ssh://production\n", "")
        with patch.dict(os.environ, {"DOCKER_CONTEXT":"production"}, clear=True), patch("preview.subprocess.run", return_value=endpoint):
            with self.assertRaises(ValueError):
                preview.docker_socket()

    def test_no_docker_cli_falls_back_to_local_socket(self):
        with tempfile.TemporaryDirectory() as home, patch.dict(os.environ, {}, clear=True), patch("preview.subprocess.run", side_effect=FileNotFoundError), patch("preview.Path.home", return_value=Path(home)):
            self.assertEqual(preview.docker_socket(), "/var/run/docker.sock")

    def test_real_hub_agent_ingestion_and_sigterm_cleanup(self):
        root = Path(__file__).resolve().parents[1]
        environment = {**os.environ, "DOCKER_HOST":"unix:///nonexistent-monitoring-preview.sock"}
        environment.pop("DOCKER_CONTEXT", None)
        process = subprocess.Popen([sys.executable, str(root / "tools/preview.py")], env=environment,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        lines = queue.Queue()
        transcript = []

        def capture():
            for line in process.stdout:
                transcript.append(line)
                lines.put(line.rstrip())

        reader = threading.Thread(target=capture, daemon=True)
        reader.start()
        directory, origin = None, None
        try:
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                line = lines.get(timeout=max(0.1, deadline-time.monotonic()))
                if line.startswith("Preview data: "):
                    directory = Path(line.removeprefix("Preview data: "))
                if line.startswith("Preview: "):
                    origin = line.removeprefix("Preview: ")
                    break
            self.assertIsNotNone(directory, "".join(transcript))
            self.assertIsNotNone(origin, "".join(transcript))
            hub = json.loads((directory / "hub.json").read_text())
            agent = json.loads((directory / "agent.json").read_text())
            # No fake samples: verify that the real agent collected host memory.
            deadline = time.monotonic() + 15
            sample = None
            while time.monotonic() < deadline:
                with sqlite3.connect(hub["database"], timeout=1) as connection:
                    row = connection.execute("SELECT data FROM hosts WHERE id=?", (agent["host_id"],)).fetchone()
                if row:
                    candidate = json.loads(row[0])
                    memory = any(metric["name"] == "memory_total_bytes" and metric["value"] > 0
                                 for metric in (candidate.get("metrics") or []))
                    if memory and candidate.get("state_at"):
                        sample = candidate
                        break
                time.sleep(0.1)
            self.assertIsNotNone(sample, "".join(transcript))
            self.assertTrue(any(metric["name"] == "memory_total_bytes" and metric["value"] > 0 for metric in sample["metrics"]))
            self.assertTrue(any("docker" in error.lower() for error in sample["errors"]), "Missing Docker must be reported, not fabricated")
            process.send_signal(signal.SIGTERM)
            self.assertEqual(process.wait(timeout=15), 0)
            reader.join(timeout=2)
            self.assertFalse(directory.exists(), "temporary credentials/database leaked")
            for listener in [hub["ui_listen"], hub["agent_listen"]]:
                host, port = listener.rsplit(":", 1)
                with socket.socket() as sock:
                    self.assertNotEqual(sock.connect_ex((host,int(port))), 0, "preview child still listening")
        finally:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=15)
            reader.join(timeout=2)
            process.stdout.close()


if __name__ == "__main__":
    unittest.main()
