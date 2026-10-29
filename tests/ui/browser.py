"""Opt-in real-browser smoke test against an isolated local hub and synthetic agents."""

import json
import math
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.request
from datetime import datetime, timedelta, timezone

from playwright.sync_api import sync_playwright


def unused_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main():
    root = Path(__file__).resolve().parents[2]
    with tempfile.TemporaryDirectory(prefix="monitoring-browser-") as directory:
        directory = Path(directory)
        ui, api = unused_port(), unused_port()
        origin = f"http://127.0.0.1:{ui}"
        tokens = {name: secrets.token_hex(32) for name in ["gateway", "erp1", "erp2", "offline"]}
        for name, token in tokens.items():
            path = directory / f"{name}.token"
            path.touch(mode=0o600)
            path.write_text(token)
        config = {
            "ui_listen": f"127.0.0.1:{ui}", "agent_listen": f"127.0.0.1:{api}",
            "public_url": origin, "database": str(directory / "hub.db"),
            "agents": {name: str(directory / f"{name}.token") for name in tokens},
        }
        config_path = directory / "hub.json"
        config_path.write_text(json.dumps(config))
        process = subprocess.Popen([str(root / "bin/monitoring"), "hub", "-config", str(config_path)])
        try:
            for _ in range(100):
                try:
                    urllib.request.urlopen(origin + "/login", timeout=1).close()
                    break
                except OSError:
                    if process.poll() is not None:
                        raise RuntimeError("hub exited before startup")
                    time.sleep(0.05)
            else:
                raise RuntimeError("hub did not start")

            def upload(host, sample_id, at, cpu):
                sample = {"version": 1, "host_id": host, "display_name": host, "id": sample_id,
                          "at": at.isoformat(), "agent_version": "browser-fixture", "errors": [],
                          "containers": [{"id": "database", "name": "database", "state": "running"}],
                          "metrics": [{"name": name, "entity": entity, "value": value, "weight": 60}
                                      for name, entity, value in [
                                          ("cpu_percent", "", cpu), ("memory_used_bytes", "", 3 * 2**30),
                                          ("memory_total_bytes", "", 4 * 2**30),
                                          ("filesystem_total_bytes", "/", 20 * 2**30),
                                          ("filesystem_free_bytes", "/", 8 * 2**30),
                                          ("network_rx_bytes_s", "eth0", 1024),
                                          ("network_tx_bytes_s", "eth0", 2048),
                                          ("container_cpu_percent", "database", 150),
                                          ("container_memory_bytes", "database", 2**28),
                                          ("uptime_seconds", "", 86400)]]}
                request = urllib.request.Request(f"http://127.0.0.1:{api}/v1/samples", json.dumps(sample).encode(),
                                                 {"Authorization": f"Bearer {tokens[host]}", "X-Host-ID": host,
                                                  "Content-Type": "application/json"})
                with urllib.request.urlopen(request, timeout=5) as response:
                    assert response.status == 204

            now = datetime.now(timezone.utc) - timedelta(seconds=2)
            for host in ["gateway", "erp1", "erp2"]:
                for i in range(48):
                    upload(host, f"sample-{i}", now - timedelta(minutes=30 * (47-i)), 30 + 20 * math.sin(i))
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(executable_path=os.getenv("MONITORING_CHROME"))
                page = browser.new_page(viewport={"width": 1440, "height": 1000})
                errors = []
                page.on("pageerror", lambda error: errors.append(str(error)))
                page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" and "favicon.ico" not in msg.text else None)
                page.goto(origin)
                assert page.title() == "keiryō · lightweight monitoring"
                brand = page.locator("header > a")
                assert brand.inner_text() == "keiryō"
                page.wait_for_function("() => document.querySelector('header img').naturalWidth > 0")
                icon = page.locator('link[rel="icon"][type="image/svg+xml"]')
                assert icon.get_attribute("href") == "/static/keiryo.svg"
                page.get_by_label("Username").fill("admin")
                page.get_by_label("Password", exact=True).fill("admin")
                page.get_by_role("button", name="Sign in", exact=True).click()
                page.wait_for_load_state()
                if not page.url.endswith("/password"):
                    raise AssertionError((page.url, page.locator("body").inner_text(), errors))
                page.get_by_label("New password", exact=False).fill("browser-test!")
                page.get_by_role("button", name="Save and sign in again").click()
                page.get_by_label("Username").fill("admin")
                page.get_by_label("Password", exact=True).fill("browser-test!")
                page.get_by_role("button", name="Sign in", exact=True).click()
                page.wait_for_selector('#overview .fleet-cell')
                page.wait_for_function("() => document.querySelector('#refresh-status').textContent.startsWith('Updated')")
                assert page.locator('.fleet-cell').count() == 4
                assert page.locator('#overview .fleet-cell.unavailable').count() == 1
                screenshot = Path(os.getenv("MONITORING_SCREENSHOT", str(directory / "fleet.png")))
                page.screenshot(path=str(screenshot), full_page=True)
                # All responsive grid declarations must parse, not silently disappear.
                page.set_viewport_size({"width": 390, "height": 844})
                assert brand.locator("img").is_visible()
                assert page.locator('.fleet-stats').evaluate("el => getComputedStyle(el).gridTemplateColumns.split(' ').length") == 2
                page.set_viewport_size({"width": 1440, "height": 1000})
                # Each navigation link renders just its view and retains the toolbar selection.
                page.get_by_label("Time range").select_option("1h")
                page.get_by_label("Filter this page").fill("erp")
                page.get_by_role("button", name="Apply", exact=True).click()
                nav = page.get_by_role("navigation", name="Dashboard sections")
                for title, section in [("Resource Views", "resources"), ("Fleet Metrics", "metrics"),
                                       ("Alerts & Hosts", "conditions"), ("Fleet Overview", "overview")]:
                    nav.get_by_role("link", name=title, exact=True).click()
                    assert page.locator("#dashboard-data > section").count() == 1
                    assert page.locator(f"#{section}").count() == 1
                    assert nav.locator('[aria-current="page"]').inner_text() == title
                    assert page.get_by_label("Time range").input_value() == "1h"
                    assert page.get_by_label("Filter this page").input_value() == "erp"
                page.go_back()
                assert page.locator("#conditions").count() == 1
                page.reload()
                assert page.locator("#conditions").count() == 1
                page.goto(origin + "/?view=resources")
                page.wait_for_selector('[data-kind="heatmap"] canvas')
                page.wait_for_function("() => document.querySelector('#refresh-status').textContent.startsWith('Updated')")
                comparison = page.locator('[data-kind="line"] .plot')
                comparison.evaluate("el => {const c=echarts.getInstanceByDom(el);c.dispatchAction({type:'dataZoom',start:25,end:75});c.dispatchAction({type:'legendUnSelect',name:'erp1'});}")
                upload("gateway", "critical", datetime.now(timezone.utc), 95)
                page.wait_for_function("() => JSON.parse(document.querySelector('[data-kind=heatmap]').dataset.series).some(p=>p.Host==='gateway' && p.Peak===95)")
                state = comparison.evaluate("el => {const o=echarts.getInstanceByDom(el).getOption();return [o.dataZoom[0].start,o.dataZoom[0].end,o.legend[0].selected.erp1];}")
                assert state == [25, 75, False], state
                nav.get_by_role("link", name="Fleet Overview", exact=True).click()
                page.wait_for_selector('.fleet-cell.critical')
                # Missed events need no replay log: reconnect refreshes from SQLite.
                page.context.set_offline(True)
                upload("erp2", "missed", datetime.now(timezone.utc), 98)
                page.context.set_offline(False)
                page.wait_for_function("() => document.querySelectorAll('.fleet-cell.critical').length===2")
                page.goto(origin + "/hosts/gateway?range=24h")
                page.wait_for_selector('.chart-panel canvas')
                page.wait_for_function("() => document.querySelector('#refresh-status').textContent.startsWith('Updated')")
                assert page.locator('.chart-panel canvas').count() == 7
                page.locator('.chart-panel .plot').first.focus()
                page.keyboard.press("End")
                assert "95" in page.locator('.chart-readout').first.inner_text()
                page.locator('#refresh-now').focus()
                page.locator('#logs').evaluate("el => el.textContent='Preserved log snapshot'")
                upload("gateway", "updated", datetime.now(timezone.utc), 40)
                page.wait_for_function("() => JSON.parse(document.querySelector('.chart-panel').dataset.series)[0].Samples.some(p=>p.Value===40)")
                assert "Preserved log snapshot" in page.locator('#logs').inner_text()
                browser.close()
                assert not errors, errors
                print("Browser smoke passed: fleet views, filters, back/reload, mobile grids, SSE/reconnect, zoom/legend, host charts, keyboard and logs")
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


if __name__ == "__main__":
    main()
