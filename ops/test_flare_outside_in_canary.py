#!/usr/bin/env python3
"""Offline contract tests for the independent Flare canary."""

import json
import os
import sys
import tempfile
import traceback
import unittest
from dataclasses import replace
from email.message import Message
from pathlib import Path
from unittest.mock import patch
from urllib import error, parse

sys.path.insert(0, str(Path(__file__).parent))
import flare_outside_in_canary as canary


CONFIG = canary.Config(
    dsn_id="42", public_key="public_canary_key_12345", project_id="project_42",
    viewer_api_key="flr_" + "a" * 48, expected_source_commit="a" * 40,
    expected_release="flare-canary-test", resend_api_key="re_resend_canary_key_12345",
    from_email="alerts@example.com", to_email="operator@example.com",
)
RECEIPT = "01K7RECEIPT1234567890ABCDE"
READY = {"status": "ok", "source_commit": "a" * 40, "release": "flare-canary-test"}


class FakeClient:
    def __init__(self, *, missing=False, fail_ready=False, ambiguous_ingest=False,
                 bad_receipt=False, bool_count=False, with_receipt_header=False,
                 missing_receipt=False, invalid_receipt=False, release_drift=False):
        self.calls = []
        self.missing = missing
        self.fail_ready = fail_ready
        self.ambiguous_ingest = ambiguous_ingest
        self.bad_receipt = bad_receipt
        self.bool_count = bool_count
        self.with_receipt_header = with_receipt_header
        self.missing_receipt = missing_receipt
        self.invalid_receipt = invalid_receipt
        self.release_drift = release_drift

    def json(self, method, path, headers=None, body=None):
        self.calls.append((method, path, headers, body))
        if path == "/readyz":
            return (503, {}, None) if self.fail_ready else (200, {},
                {**READY, "release": "unexpected"} if self.release_drift else READY)
        if method == "POST":
            if self.ambiguous_ingest and sum(c[0] == "POST" for c in self.calls) == 1:
                raise canary.CanaryFailure("public_transport")
            result = {"accepted": True if self.bool_count else 1, "rejected": 0,
                      "dropped": 0, "receipt_id": RECEIPT}
            headers = {"X-Flare-Receipt-ID": "wrong" if self.bad_receipt else RECEIPT} if \
                self.bad_receipt or self.with_receipt_header else {}
            return 200, headers, result
        if path == "/api/projects/project_42/ingest-receipts?limit=10":
            receipt = {"id": RECEIPT, "pillar": "logs", "protocol": "native",
                       "outcome": "accepted", "completed_at": "2026-10-04T00:00:00Z",
                       "received_count": 1, "accepted_count": 1, "persisted_count": 1,
                       "rejected_count": 0, "dropped_count": 0, "http_status": 200,
                       "retryable": False}
            if self.invalid_receipt:
                receipt["persisted_count"] = 0
            return 200, {}, [] if self.missing_receipt else [receipt]
        assert path.startswith("/api/projects/project_42/logs?")
        assert parse.parse_qs(parse.urlparse(path).query)["q"] == ["flare-canary-"]
        marker = json.loads(next(call[3] for call in self.calls if call[0] == "POST"))[0]["body"]
        return 200, {}, {"logs": [] if self.missing else [{"body": marker}]}


class CanaryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.state = Path(self.temp.name) / "state.json"
        self.mail = []
        self.send = lambda _cfg, subject, body: self.mail.append((subject, body))

    def test_full_cycle_matches_same_project_marker_and_receipt(self):
        client = FakeClient()
        result = canary.cycle(CONFIG, client, self.state, send=self.send, now=lambda: 10,
                              sleep=lambda _: None)
        self.assertEqual(result, "ok")
        self.assertEqual(self.mail, [])
        self.assertEqual([call[0] for call in client.calls], ["GET", "POST", "GET", "GET"])
        post = client.calls[1]
        self.assertEqual(post[1], "/api/42/logs")
        self.assertEqual(post[2]["X-Flare-Key"], CONFIG.public_key)
        marker = json.loads(post[3])[0]["body"]
        self.assertEqual(post[2]["Idempotency-Key"], marker)
        self.assertEqual(client.calls[2][1], "/api/projects/project_42/ingest-receipts?limit=10")
        self.assertEqual(parse.parse_qs(parse.urlparse(client.calls[3][1]).query)["q"], ["flare-canary-"])
        self.assertNotIn(marker, client.calls[3][1])
        self.assertEqual(client.calls[2][2]["Authorization"], "Bearer " + CONFIG.viewer_api_key)
        self.assertEqual(client.calls[3][2]["Authorization"], "Bearer " + CONFIG.viewer_api_key)
        self.assertEqual(json.loads(self.state.read_text())["status"], "ok")

    def test_optional_deadman_pings_only_after_complete_readback(self):
        canary.cycle(CONFIG, FakeClient(), self.state, send=self.send,
                     sleep=lambda _: None, ping=lambda _url: self.fail("disabled ping called"))
        config = replace(CONFIG, deadman_url="https://deadman.example/ping/private-token")
        client = FakeClient()
        pings = []

        def ping(url):
            pings.append(url)
            self.assertEqual([call[0] for call in client.calls], ["GET", "POST", "GET", "GET"])

        self.assertEqual(canary.cycle(config, client, self.state, send=self.send,
                                      sleep=lambda _: None, ping=ping), "ok")
        self.assertEqual(pings, [config.deadman_url])
        for bad_client in (FakeClient(fail_ready=True), FakeClient(missing_receipt=True),
                           FakeClient(missing=True)):
            pings.clear()
            result = canary.cycle(config, bad_client, self.state, send=self.send,
                                  sleep=lambda _: None, ping=ping)
            self.assertNotEqual(result, "ok")
            self.assertEqual(pings, [])

    def test_failed_deadman_pings_alert_and_fail_cycle(self):
        secret_url = "https://deadman.example/ping/private-token"
        config = replace(CONFIG, deadman_url=secret_url)

        def fail_ping(_url):
            raise canary.CanaryFailure("deadman_unavailable")

        self.assertEqual(canary.cycle(config, FakeClient(), self.state, send=self.send,
                                      now=lambda: 10, sleep=lambda _: None,
                                      ping=fail_ping), "deadman_unavailable")
        self.assertEqual(json.loads(self.state.read_text())["status"], "failed")
        self.assertIn("deadman_unavailable", self.mail[0][1])
        self.assertNotIn(secret_url, self.mail[0][1])
        self.assertEqual(canary.cycle(config, FakeClient(), self.state, send=self.send,
                                      now=lambda: 20, sleep=lambda _: None,
                                      ping=lambda _url: None), "ok")
        self.assertIn("recovered", self.mail[-1][0])

    def test_ambiguous_ingest_reuses_exact_bytes_and_key(self):
        client = FakeClient(ambiguous_ingest=True)
        canary.probe_once(CONFIG, client, marker="flare-canary-" + "f" * 32, sleep=lambda _: None)
        posts = [call for call in client.calls if call[0] == "POST"]
        self.assertEqual(len(posts), 2)
        self.assertEqual(posts[0], posts[1])

    def test_first_failure_dedup_reminder_and_recovery(self):
        for at in (10, 20, canary.REPEAT_ALERT_SECONDS + 11):
            result = canary.cycle(CONFIG, FakeClient(fail_ready=True), self.state,
                                  send=self.send, now=lambda at=at: at, sleep=lambda _: None)
            self.assertEqual(result, "readiness")
        self.assertEqual(len(self.mail), 2)
        self.assertTrue(all("readiness" in body for _, body in self.mail))
        self.assertTrue(all(CONFIG.public_key not in body and "flare-canary-" not in body
                            for _, body in self.mail))
        self.assertEqual(canary.cycle(CONFIG, FakeClient(), self.state, send=self.send,
                                      now=lambda: 30000, sleep=lambda _: None), "ok")
        self.assertEqual(len(self.mail), 3)
        self.assertIn("recovered", self.mail[-1][0])

    def test_missing_query_and_bad_receipt_fail(self):
        for client, expected in ((FakeClient(missing=True), "query_missing"),
                                 (FakeClient(missing_receipt=True), "receipt_missing"),
                                 (FakeClient(invalid_receipt=True), "receipt_invalid"),
                                 (FakeClient(bad_receipt=True), "ingest_acceptance"),
                                 (FakeClient(bool_count=True), "ingest_acceptance")):
            with self.subTest(expected=expected, client=vars(client)):
                with self.assertRaisesRegex(canary.CanaryFailure, expected):
                    canary.probe_once(CONFIG, client, sleep=lambda _: None)

    def test_exact_release_identity_mismatch_fails_before_ingest(self):
        client = FakeClient(release_drift=True)
        with self.assertRaisesRegex(canary.CanaryFailure, "release_identity"):
            canary.probe_once(CONFIG, client, sleep=lambda _: None)
        self.assertEqual([call[0] for call in client.calls], ["GET"])

    def test_config_requires_private_regular_file_and_strings(self):
        path = Path(self.temp.name) / "config.json"
        path.write_text(json.dumps(vars(CONFIG)))
        os.chmod(path, 0o600)
        self.assertEqual(canary.read_config(path), CONFIG)
        os.chmod(path, 0o644)
        with self.assertRaisesRegex(canary.CanaryFailure, "config_permissions"):
            canary.read_config(path)
        os.chmod(path, 0o600)
        data = vars(CONFIG).copy()
        data["dsn_id"] = 42
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(canary.CanaryFailure, "config_invalid"):
            canary.read_config(path)
        data = vars(CONFIG).copy()
        data["viewer_api_key"] = "flare_" + "a" * 48
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(canary.CanaryFailure, "config_invalid"):
            canary.read_config(path)
        data = vars(CONFIG).copy()
        data["expected_source_commit"] = "pending"
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(canary.CanaryFailure, "config_invalid"):
            canary.read_config(path)

    def test_deadman_config_is_optional_and_https_only(self):
        path = Path(self.temp.name) / "config.json"
        os.chmod(self.temp.name, 0o700)
        data = vars(CONFIG).copy()
        del data["deadman_url"]
        path.write_text(json.dumps(data))
        os.chmod(path, 0o600)
        self.assertIsNone(canary.read_config(path).deadman_url)
        for url in (None, "https://deadman.example/ping/private-token?key=abc"):
            data["deadman_url"] = url
            path.write_text(json.dumps(data))
            self.assertEqual(canary.read_config(path).deadman_url, url)
        for url in ("", "http://deadman.example/ping", "https://",
                    "https://user:pass@deadman.example/ping", "https://deadman.example/ping#frag",
                    "https://deadman.example/ping\nsecret", "https://deadman.example\\@evil.example/ping",
                    "https://deadman.example:invalid/ping", 42):
            with self.subTest(url=url):
                data["deadman_url"] = url
                path.write_text(json.dumps(data))
                with self.assertRaisesRegex(canary.CanaryFailure, "config_invalid"):
                    canary.read_config(path)

    def test_deadman_ping_has_bounded_https_request_and_secret_free_errors(self):
        secret_url = "https://deadman.example/ping/private-token"

        class Response:
            def __init__(self, status):
                self.status = status

            def __enter__(self):
                return self

            def __exit__(self, *_):
                pass

        class Opener:
            def __init__(self, result):
                self.result = result
                self.calls = []

            def open(self, req, timeout):
                self.calls.append((req.full_url, req.get_method(), timeout))
                if isinstance(self.result, Exception):
                    raise self.result
                return Response(self.result)

        opener = Opener(204)
        canary.ping_deadman(secret_url, opener=opener)
        self.assertEqual(opener.calls, [(secret_url, "GET", canary.DEADMAN_TIMEOUT_SECONDS)])
        handlers = []

        def build_opener(*args):
            handlers.extend(args)
            return Opener(204)

        with patch.object(canary.request, "build_opener", side_effect=build_opener):
            canary.ping_deadman(secret_url)
        self.assertTrue(any(isinstance(handler, canary.NoRedirect) for handler in handlers))
        self.assertEqual(next(handler.proxies for handler in handlers
                              if isinstance(handler, canary.request.ProxyHandler)), {})
        for failure in (302, 500, error.URLError(secret_url),
                        error.HTTPError(secret_url, 302, "redirect", {}, None)):
            with self.subTest(failure=failure):
                with self.assertRaises(canary.CanaryFailure) as caught:
                    canary.ping_deadman(secret_url, opener=Opener(failure))
                self.assertEqual(str(caught.exception), "deadman_unavailable")
                self.assertIsNone(caught.exception.__context__)
                formatted = "".join(traceback.format_exception(caught.exception))
                self.assertNotIn(secret_url, formatted)

    def test_mail_starts_tls_before_authentication(self):
        events = []

        class SMTP:
            def __init__(self, host, port, timeout):
                self.assert_args = (host, port, timeout)

            def __enter__(self):
                events.append(("connect", self.assert_args))
                return self

            def __exit__(self, *_):
                pass

            def starttls(self, context):
                events.append(("tls", context is not None))

            def login(self, user, secret):
                events.append(("login", user, secret))

            def send_message(self, message):
                events.append(("send", message["To"]))

        original = canary.smtplib.SMTP
        canary.smtplib.SMTP = SMTP
        try:
            canary.send_mail(CONFIG, "Test", "Public canary alert")
        finally:
            canary.smtplib.SMTP = original
        self.assertEqual([event[0] for event in events], ["connect", "tls", "login", "send"])
        self.assertEqual(events[0][1], ("smtp.resend.com", 587, 10))
        self.assertEqual(events[-1][1], CONFIG.to_email)

    def test_http_client_is_fixed_origin_and_requires_ingress_marker(self):
        class Response:
            status = 200

            def __init__(self, marker):
                self.headers = Message()
                self.headers["Content-Type"] = "application/json"
                if marker:
                    self.headers[canary.INGRESS_HEADER] = canary.INGRESS_VALUE

            def __enter__(self):
                return self

            def __exit__(self, *_):
                pass

            def read(self, _limit):
                return b'{"status":"ok"}'

        class Opener:
            def __init__(self, marker):
                self.marker = marker
                self.urls = []

            def open(self, req, timeout):
                self.urls.append((req.full_url, timeout))
                return Response(self.marker)

        client = canary.PublicFlareClient()
        client.opener = Opener(True)
        self.assertEqual(client.json("GET", "/readyz")[0], 200)
        self.assertEqual(client.opener.urls,
                         [("https://flare.cloudrebellion.tech/readyz", 5)])
        client.opener = Opener(False)
        with self.assertRaisesRegex(canary.CanaryFailure, "public_ingress"):
            client.json("GET", "/readyz")
        self.assertIsNone(canary.NoRedirect().redirect_request(None, None, 302, "", {},
                                                                 "https://other.example/"))


if __name__ == "__main__":
    unittest.main()
