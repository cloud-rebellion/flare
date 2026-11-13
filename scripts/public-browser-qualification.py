#!/usr/bin/env python3
"""Run the 28 built-static UI route cases against one owned loopback server.

The API replies in the browser fixture are synthetic. This verifies launcher
behavior, not a provider sign-in or a production account/session journey.
"""

import argparse
import functools
import http.server
import json
import os
import pathlib
import signal
import subprocess
import threading
import urllib.parse

ROOT = pathlib.Path(__file__).resolve().parents[1]
EXPECTED_TESTS = 28
MAX_REPORT_BYTES = 2 * 1024 * 1024
STARTING_CHILD = False
PENDING_SIGNAL = None


def interrupted(signum, _frame):
    global PENDING_SIGNAL
    if STARTING_CHILD:
        if PENDING_SIGNAL is None:
            PENDING_SIGNAL = signum
        return
    raise SystemExit(128 + signum)


def main():
    global STARTING_CHILD
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", required=True, type=pathlib.Path)
    args = parser.parse_args()
    if pathlib.Path.cwd().resolve() != ROOT:
        raise RuntimeError("run from the public product root")
    if (ROOT / "flare").exists():
        raise RuntimeError("run from the public-only exported tree, without nested private-layout leftovers")
    output = args.output_dir.resolve()
    if output == ROOT or ROOT in output.parents:
        raise RuntimeError("browser output must be outside authored source")
    output.mkdir(parents=True, exist_ok=True)
    static = (ROOT / "frontend/build").resolve()
    if not (static / "index.html").is_file():
        raise RuntimeError("the actual compiled frontend is required")
    if subprocess.check_output(["node", "--version"], text=True).strip() != "v24.22.2":
        raise RuntimeError("Node 24.22.2 is required")
    version = subprocess.check_output(
        ["node", "-p", "require('./browser-tests/node_modules/@playwright/test/package.json').version"],
        cwd=ROOT, text=True,
    ).strip()
    if version != "1.59.1":
        raise RuntimeError("frozen Playwright 1.59.1 is required")

    class Static(http.server.SimpleHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            path = urllib.parse.unquote(urllib.parse.urlsplit(self.path).path)
            target = (static / path.lstrip("/")).resolve()
            if not target.is_relative_to(static):
                self.send_error(404)
                return
            if path.startswith("/api/"):
                self.send_error(503, "only intercepted synthetic API replies are supported")
                return
            if not target.is_file():
                self.path = "/index.html"
            super().do_GET()

    handler = functools.partial(Static, directory=str(static))
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    report = output / "playwright.json"
    env = dict(os.environ)
    env.update({
        "FLARE_BROWSER_BASE_URL": "http://127.0.0.1:" + str(server.server_address[1]),
        "FLARE_BROWSER_REPORT": str(report),
        "FLARE_BROWSER_OUTPUT": str(output / "browser-output"),
    })
    process = None
    try:
        STARTING_CHILD = True
        try:
            process = subprocess.Popen([
                "node", str(ROOT / "browser-tests/node_modules/@playwright/test/cli.js"),
                "test", "--config", str(ROOT / "browser-tests/playwright.config.ts"),
            ], cwd=ROOT, env=env, start_new_session=True)
        finally:
            STARTING_CHILD = False
        if PENDING_SIGNAL is not None:
            raise SystemExit(128 + PENDING_SIGNAL)
        try:
            code = process.wait(timeout=210)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGINT)
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=10)
            raise RuntimeError("owned browser qualification timed out")
        if not report.is_file() or report.stat().st_size > MAX_REPORT_BYTES:
            raise RuntimeError("bounded browser result is missing")
        result = json.loads(report.read_text())
        tests = []

        def collect(suite):
            for spec in suite.get("specs", []):
                tests.extend(spec.get("tests", []))
            for child in suite.get("suites", []):
                collect(child)

        for suite in result.get("suites", []):
            collect(suite)
        stats = result.get("stats", {})
        passed = code == 0 and not result.get("errors") and len(tests) == EXPECTED_TESTS
        passed = passed and stats.get("expected") == EXPECTED_TESTS
        passed = passed and all(stats.get(key) == 0 for key in ("unexpected", "skipped", "flaky"))
        passed = passed and all(
            item.get("expectedStatus") == "passed"
            and item.get("status") == "expected"
            and len(item.get("results", [])) == 1
            and item["results"][0].get("status") == "passed"
            and item["results"][0].get("retry") == 0
            for item in tests
        )
        receipt = {
            "status": "passed" if passed else "failed",
            "expected": EXPECTED_TESTS,
            "stats": stats,
            "retries": sum(item.get("retry", 0) for test in tests for item in test.get("results", [])),
            "boundary": "actual compiled UI and synthetic API replies; not provider SSO",
            "interactive_profile_used": False,
        }
    finally:
        if process is not None and process.poll() is None:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=10)
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
        if thread.is_alive():
            raise RuntimeError("owned loopback server did not close")
    receipt["server_closed"] = True
    (output / "qualification.json").write_text(json.dumps(receipt, indent=2) + "\n")
    print(json.dumps(receipt), flush=True)
    if not passed:
        raise RuntimeError("exact 28-case built-static browser qualification did not pass")


if __name__ == "__main__":
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    main()
