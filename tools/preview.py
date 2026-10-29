"""Run the native monitoring binary as an isolated local hub and real agent."""

import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import urllib.request


def docker_socket():
    context = os.environ.get("DOCKER_CONTEXT")
    endpoint = os.environ.get("DOCKER_HOST") if not context else None
    if not endpoint:
        try:
            result = subprocess.run(["docker", "context", "inspect", *([context] if context else []),
                                     "--format", "{{.Endpoints.docker.Host}}"],
                                    capture_output=True, text=True, timeout=5, check=True)
            endpoint = result.stdout.strip()
        except (OSError, subprocess.SubprocessError):
            if context:
                raise ValueError("Cannot resolve DOCKER_CONTEXT; select a local Docker context") from None
    if endpoint:
        url = urllib.parse.urlsplit(endpoint)
        if url.scheme != "unix" or url.netloc or not url.path.startswith("/") or url.query or url.fragment:
            raise ValueError("Preview only supports local Unix Docker sockets; clear DOCKER_HOST or select a local DOCKER_CONTEXT")
        return urllib.parse.unquote(url.path)
    desktop = Path.home() / ".docker/run/docker.sock"
    return str(desktop) if desktop.is_socket() else "/var/run/docker.sock"


def write_configs(directory, ui_port, agent_port, docker_path):
    token = directory / "preview.token"
    with os.fdopen(os.open(token, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as file:
        file.write(secrets.token_hex(32))
    hub = {
        "ui_listen": f"127.0.0.1:{ui_port}", "agent_listen": f"127.0.0.1:{agent_port}",
        "public_url": f"http://127.0.0.1:{ui_port}", "auth_mode": "local",
        "database": str(directory / "preview.db"), "agents": {"preview-local": str(token)},
    }
    agent = {
        "host_id": "preview-local", "display_name": f"{socket.gethostname()} (local preview)"[:256],
        "hub_url": f"http://127.0.0.1:{agent_port}", "credential_file": str(token),
        "docker_socket": docker_path,
        "queue_database": str(directory / "agent.redb"),
        # macOS devfs reports 100% full despite being a virtual device filesystem.
        "exclude_filesystems": ["devfs"],
    }
    for name, config in [("hub.json", hub), ("agent.json", agent)]:
        with os.fdopen(os.open(directory / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as file:
            json.dump(config, file)
    return directory / "hub.json", directory / "agent.json"


def stop_processes(processes):
    # Stop the agent first so it cannot upload during hub shutdown.
    for process in reversed(processes):
        if process.poll() is None:
            process.terminate()
    for process in reversed(processes):
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def main():
    binary = Path(__file__).resolve().parents[1] / "bin/monitoring"
    if not binary.is_file():
        raise RuntimeError("Build the native binary first: task build TARGET=native")
    docker_path = docker_socket()
    stopping = threading.Event()
    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, lambda *_: stopping.set())
    # Reserve distinct ephemeral loopback ports together; fail clearly if another process wins the handoff.
    with socket.socket() as ui, socket.socket() as api:
        ui.bind(("127.0.0.1", 0))
        api.bind(("127.0.0.1", 0))
        ui_port, agent_port = ui.getsockname()[1], api.getsockname()[1]
    origin = f"http://127.0.0.1:{ui_port}"
    with tempfile.TemporaryDirectory(prefix="monitoring-preview-") as temporary:
        directory = Path(temporary)
        hub_config, agent_config = write_configs(directory, ui_port, agent_port, docker_path)
        environment = {key: value for key, value in os.environ.items()
                       if key not in {"HOST_PROC", "HOST_SYS", "HOST_ETC", "HOST_ROOT"}}
        processes = []
        try:
            processes.append(subprocess.Popen([str(binary), "hub", "-config", str(hub_config)], env=environment))
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            deadline = time.monotonic() + 15
            while not stopping.is_set():
                if processes[0].poll() is not None:
                    raise RuntimeError("Preview hub exited before startup; see its error above")
                try:
                    with opener.open(origin + "/login", timeout=1):
                        break
                except OSError:
                    if time.monotonic() >= deadline:
                        raise RuntimeError("Preview hub did not become ready within 15 seconds") from None
                    stopping.wait(0.1)
            if stopping.is_set():
                return
            processes.append(subprocess.Popen([str(binary), "agent", "-config", str(agent_config)], env=environment))
            print(f"Preview data: {directory}", flush=True)
            print(f"Preview: {origin}", flush=True)
            print("Sign in: admin / admin, then set a preview-only password. Ctrl+C stops both processes.", flush=True)
            print("Collection: default 60 seconds. CPU/rate readings need two samples; history fills as you leave it running.", flush=True)
            print(f"Docker socket: {docker_path}", flush=True)
            if not Path(docker_path).is_socket():
                print("Docker socket unavailable; real host metrics still work and Docker collection errors remain visible.", flush=True)
            while not stopping.wait(0.25):
                for mode, process in zip(("hub", "agent"), processes):
                    if process.poll() is not None:
                        raise RuntimeError(f"Preview {mode} exited with status {process.returncode}; see its error above")
        finally:
            stop_processes(processes)
    print("Preview stopped; temporary credentials and history removed.", flush=True)


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, ValueError) as error:
        print(f"Preview failed: {error}", file=sys.stderr)
        sys.exit(1)
