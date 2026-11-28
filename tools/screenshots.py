"""Regenerate README screenshots from an isolated hub seeded with a synthetic fleet.

Run with `uv run --locked task screenshots`. Nothing here touches real hosts:
the hub binds to loopback in a temporary directory and the fleet is fabricated.
"""

import base64
import json
import math
import os
from pathlib import Path
import random
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.request
from datetime import datetime, timedelta, timezone

from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "docs" / "images"
GiB = 2**30
PASSWORD = "screenshots!"

# host -> (display name, cpu base, cpu swing, mem used/total GiB, disk used/total GiB, containers)
FLEET = {
    "gateway": ("gateway", 12, 6, (1.6, 4), (9, 40), [
        ("traefik", "running", "healthy"), ("authentik-server", "running", "healthy"),
        ("authentik-worker", "running", ""), ("tailscale", "running", "")]),
    "nas": ("nas", 18, 8, (5.1, 16), (3400, 4000), [
        ("samba", "running", ""), ("restic", "exited", ""), ("syncthing", "running", "healthy")]),
    "media": ("media", 38, 26, (9.8, 16), (610, 1000), [
        ("jellyfin", "running", "healthy"), ("sonarr", "running", ""), ("radarr", "running", ""),
        ("qbittorrent", "running", "")]),
    "db": ("db", 24, 10, (13.4, 16), (120, 250), [
        ("postgres", "running", "healthy"), ("redis", "running", "healthy")]),
    "erp": ("erp", 31, 14, (5.9, 12), (64, 160), [
        ("erpnext", "running", "healthy"), ("erpnext-worker", "restarting", ""),
        ("mariadb", "running", "healthy")]),
    "edge-vps": ("edge-vps", 7, 4, (0.6, 1), (11, 50), [
        ("caddy", "running", ""), ("uptime-probe", "running", "")]),
}


def unused_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_for(origin, process):
    for _ in range(200):
        try:
            urllib.request.urlopen(origin + "/login", timeout=1).close()
            return
        except OSError:
            if process.poll() is not None:
                raise RuntimeError("hub exited before startup")
            time.sleep(0.05)
    raise RuntimeError("hub did not start")


def seed(api, tokens):
    rng = random.Random(7)
    now = datetime.now(timezone.utc) - timedelta(seconds=5)
    steps = 144  # 24h at 10-minute intervals
    for host, (name, cpu_base, swing, (mem, mem_total), (used, total), containers) in FLEET.items():
        for i in range(steps):
            at = now - timedelta(minutes=10 * (steps - 1 - i))
            daily = math.sin((i / steps) * 2 * math.pi - 1.2)
            cpu = cpu_base + swing * daily + rng.uniform(-3, 3)
            if host == "media" and 96 <= i <= 110:
                cpu += 52  # an evening transcode
            if host == "db" and i == steps - 1:
                cpu = 93  # sustained critical spike in the latest reading
            cpu = max(1.0, min(99.0, cpu))
            mem_used = mem * GiB * (1 + 0.04 * daily + rng.uniform(-0.01, 0.01))
            rx = (2e5 + 1.5e6 * max(daily, 0)) * (6 if host == "media" else 1) * rng.uniform(0.7, 1.3)
            tx = rx * (2.5 if host == "media" else 0.4)
            metrics = [
                ("cpu_percent", "", cpu), ("memory_used_bytes", "", mem_used),
                ("memory_total_bytes", "", mem_total * GiB),
                ("filesystem_total_bytes", "/", total * GiB),
                ("filesystem_free_bytes", "/", (total - used - 0.05 * i) * GiB),
                ("network_rx_bytes_s", "eth0", rx), ("network_tx_bytes_s", "eth0", tx),
                ("uptime_seconds", "", 86400 * 12 + 600 * i),
            ]
            states = []
            for cname, state, health in containers:
                exit_code = 0 if state == "exited" else None
                states.append({"id": cname, "name": cname, "state": state, "health": health,
                               "restarts": 4 if state == "restarting" else 0,
                               **({"exit_code": exit_code} if exit_code is not None else {})})
                if state == "running":
                    metrics.append(("container_cpu_percent", cname,
                                    max(0.2, cpu / len(containers) * rng.uniform(0.5, 1.5))))
                    metrics.append(("container_memory_bytes", cname,
                                    mem_used / (len(containers) + 1) * rng.uniform(0.6, 1.2)))
            sample = {
                "version": 1, "host_id": host, "display_name": name, "id": f"{host}-{i}",
                "at": at.isoformat(), "agent_version": "0.1.0", "errors": [], "containers": states,
                "metrics": [{"name": n, "entity": e, "value": v, "weight": 600} for n, e, v in metrics],
            }
            request = urllib.request.Request(
                f"http://127.0.0.1:{api}/v1/samples", json.dumps(sample).encode(),
                {"Authorization": f"Bearer {tokens[host]}", "X-Host-ID": host,
                 "Content-Type": "application/json"})
            with urllib.request.urlopen(request, timeout=5) as response:
                assert response.status == 204, response.status


def sign_in(page, origin):
    page.goto(origin)
    page.get_by_label("Username").fill("admin")
    page.get_by_label("Password", exact=True).fill("admin")
    page.get_by_role("button", name="Sign in", exact=True).click()
    page.wait_for_load_state()
    page.get_by_label("New password", exact=False).fill(PASSWORD)
    page.get_by_role("button", name="Save and sign in again").click()
    page.get_by_label("Username").fill("admin")
    page.get_by_label("Password", exact=True).fill(PASSWORD)
    page.get_by_role("button", name="Sign in", exact=True).click()
    page.wait_for_selector("#overview .host-card")


def settle(page):
    page.wait_for_function(
        "() => document.querySelector('#refresh-status').textContent.startsWith('Updated')")
    page.wait_for_timeout(1200)  # let ECharts finish its entry animation


def social_card(page, hero):
    image = base64.b64encode(hero.read_bytes()).decode()
    logo = (ROOT / "internal/hub/static/keiryo.svg").read_text()
    page.set_viewport_size({"width": 1280, "height": 640})
    page.set_content(f"""<!doctype html><html><body style="margin:0;width:1280px;height:640px;
      background:#111217;color:#e4e7ef;font-family:system-ui,sans-serif;overflow:hidden;position:relative">
      <div style="position:absolute;left:72px;top:96px;width:520px">
        <div style="display:flex;align-items:center;gap:20px">
          <div style="width:88px;height:88px">{logo}</div>
          <div style="font-size:84px;font-weight:700;letter-spacing:-2px">keiryō</div>
        </div>
        <p style="font-size:34px;line-height:1.3;margin:40px 0 0;color:#ccccdc">
          Lightweight self-hosted monitoring for servers &amp; Docker.</p>
        <p style="font-size:24px;margin:36px 0 0;color:#5794f2">
          Go hub · Rust agents · OpenTelemetry · SQLite</p>
      </div>
      <img src="data:image/png;base64,{image}" style="position:absolute;left:640px;top:72px;width:900px;
        border:1px solid #2c3235;border-radius:14px;box-shadow:0 20px 60px rgba(0,0,0,.6)">
    </body></html>""")
    page.wait_for_function("() => document.querySelector('img').complete")
    page.screenshot(path=str(OUT / "social-preview.png"))


def main():
    binary = ROOT / "bin/monitoring"
    if not binary.exists():
        raise SystemExit("bin/monitoring missing; run `task build TARGET=native` first")
    OUT.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="keiryo-screenshots-") as directory:
        directory = Path(directory)
        ui, api = unused_port(), unused_port()
        origin = f"http://127.0.0.1:{ui}"
        tokens = {host: secrets.token_hex(32) for host in FLEET}
        for host, token in tokens.items():
            path = directory / f"{host}.token"
            path.touch(mode=0o600)
            path.write_text(token)
        config = {
            "ui_listen": f"127.0.0.1:{ui}", "agent_listen": f"127.0.0.1:{api}",
            "public_url": origin, "database": str(directory / "hub.db"),
            "agents": {host: str(directory / f"{host}.token") for host in tokens},
            "alert_pending_seconds": 0,
        }
        (directory / "hub.json").write_text(json.dumps(config))
        process = subprocess.Popen([str(binary), "hub", "-config", str(directory / "hub.json")],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            wait_for(origin, process)
            seed(api, tokens)
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(executable_path=os.getenv("MONITORING_CHROME"))
                page = browser.new_page(viewport={"width": 1440, "height": 900}, device_scale_factor=2)
                sign_in(page, origin)
                shots = [
                    ("overview", "/?view=overview&range=24h"),
                    ("resources", "/?view=resources&range=24h"),
                    ("metrics", "/?view=metrics&range=24h"),
                    ("conditions", "/?view=conditions&range=24h"),
                    ("host", "/hosts/media?range=24h"),
                ]
                for name, path in shots:
                    page.goto(origin + path)
                    settle(page)
                    page.screenshot(path=str(OUT / f"{name}.png"))
                    print(f"wrote docs/images/{name}.png")
                card = browser.new_page(device_scale_factor=1)
                social_card(card, OUT / "overview.png")
                print("wrote docs/images/social-preview.png")
                browser.close()
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


if __name__ == "__main__":
    main()
