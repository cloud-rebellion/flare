#!/usr/bin/env python3
"""One outside-in Flare acceptance cycle. Install only on an independent host."""

import argparse
import fcntl
import json
import os
import re
import secrets
import smtplib
import ssl
import stat
import sys
import tempfile
import time
from dataclasses import dataclass
from email.message import EmailMessage
from pathlib import Path
from typing import Optional
from urllib import error, parse, request


ORIGIN = "https://flare.cloudrebellion.tech"
INGRESS_HEADER = "X-CR-Flare-Overlap"
INGRESS_VALUE = "nginx-direct-host-preserved-v1"
SMTP_HOST = "smtp.resend.com"
SMTP_PORT = 587
REPEAT_ALERT_SECONDS = 6 * 60 * 60
MAX_RESPONSE_BYTES = 64 * 1024
MAX_CONFIG_BYTES = 4096
DEADMAN_TIMEOUT_SECONDS = 5


class CanaryFailure(Exception):
    """Carries one safe operator code; never a URL, response, or credential."""


@dataclass(frozen=True)
class Config:
    dsn_id: str
    public_key: str
    project_id: str
    viewer_api_key: str
    expected_source_commit: str
    expected_release: str
    resend_api_key: str
    from_email: str
    to_email: str
    deadman_url: Optional[str] = None


def _single_email(value):
    return isinstance(value, str) and len(value) < 255 and "@" in value and not re.search(r"[\s,;<>]", value)


def _valid_deadman_url(value):
    if value is None:
        return True
    # URLs often contain a bearer-like secret in the path. Validate without
    # ever including the value in an exception, log, or alert.
    if not isinstance(value, str) or not 0 < len(value) <= 2048 or \
       any(not 0x21 <= ord(char) <= 0x7e or char in "\\#" for char in value):
        return False
    try:
        parts = parse.urlsplit(value)
        return parts.scheme == "https" and bool(parts.hostname) and \
            parts.username is None and parts.password is None and \
            not parts.fragment and parts.port != 0
    except ValueError:
        return False


def read_config(path):
    file = Path(path)
    info = file.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > MAX_CONFIG_BYTES:
        raise CanaryFailure("config_permissions")
    with file.open("rb") as handle:
        raw = handle.read(MAX_CONFIG_BYTES + 1)
    if len(raw) > MAX_CONFIG_BYTES:
        raise CanaryFailure("config_size")
    try:
        data = json.loads(raw)
        config = Config(**data)
    except (ValueError, TypeError) as exc:
        raise CanaryFailure("config_invalid") from exc
    if any(not isinstance(value, str) for key, value in vars(config).items() if key != "deadman_url") or \
       not re.fullmatch(r"[0-9]{1,20}", config.dsn_id) or not re.fullmatch(r"[A-Za-z0-9_-]{16,128}", config.public_key) or \
       not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", config.project_id) or \
       not re.fullmatch(r"flr_[a-f0-9]{48}", config.viewer_api_key) or \
       not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", config.expected_source_commit) or \
       not (0 < len(config.expected_release) <= 128) or \
       not re.fullmatch(r"re_[A-Za-z0-9_-]{16,256}", config.resend_api_key) or \
       not _single_email(config.from_email) or not _single_email(config.to_email) or \
       not _valid_deadman_url(config.deadman_url):
        raise CanaryFailure("config_invalid")
    return config


def _header(headers, name):
    return next((str(v) for k, v in headers.items() if k.lower() == name.lower()), "")


class NoRedirect(request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class PublicFlareClient:
    def __init__(self):
        self.opener = request.build_opener(request.ProxyHandler({}), request.HTTPSHandler(context=ssl.create_default_context()), NoRedirect())

    def json(self, method, path, headers=None, body=None):
        # Paths are assembled only from validated IDs and fixed route names.
        if not path.startswith("/") or path.startswith("//"):
            raise CanaryFailure("invalid_request_path")
        req = request.Request(ORIGIN + path, data=body, headers=headers or {}, method=method)
        try:
            with self.opener.open(req, timeout=5) as response:
                status = response.status
                response_headers = response.headers
                raw = response.read(MAX_RESPONSE_BYTES + 1)
        except error.HTTPError as exc:
            # Do not read or log a possibly sensitive server response body.
            try:
                return exc.code, dict(exc.headers or {}), None
            finally:
                exc.close()
        except (error.URLError, TimeoutError, OSError) as exc:
            raise CanaryFailure("public_transport") from exc
        if len(raw) > MAX_RESPONSE_BYTES or _header(response_headers, INGRESS_HEADER) != INGRESS_VALUE:
            raise CanaryFailure("public_ingress")
        if _header(response_headers, "Content-Type").split(";", 1)[0].strip() != "application/json":
            raise CanaryFailure("public_content_type")
        try:
            decoded = json.loads(raw)
        except ValueError as exc:
            raise CanaryFailure("public_json") from exc
        return status, response_headers, decoded


def _ready(client, config):
    status, _, body = client.json("GET", "/readyz")
    if status != 200 or not isinstance(body, dict) or body.get("status") != "ok":
        raise CanaryFailure("readiness")
    if body.get("source_commit") != config.expected_source_commit or \
       body.get("release") != config.expected_release:
        raise CanaryFailure("release_identity")


def _ingest(client, config, marker, sleep):
    payload = json.dumps([{"severity": "info", "body": marker,
                           "attributes": {"source": "outside-in-canary"}}], separators=(",", ":")).encode()
    path = "/api/" + config.dsn_id + "/logs"
    headers = {"Content-Type": "application/json", "X-Flare-Key": config.public_key,
               "Idempotency-Key": marker}
    for attempt in range(2):
        try:
            status, response_headers, body = client.json("POST", path, headers, payload)
        except CanaryFailure as exc:
            if attempt == 0 and str(exc) == "public_transport":
                sleep(1)
                continue
            raise CanaryFailure("ingest_transport") from exc
        if status == 503 and attempt == 0:
            sleep(1)
            continue
        if status != 200 or not isinstance(body, dict) or \
           type(body.get("accepted")) is not int or body["accepted"] != 1 or \
           type(body.get("rejected")) is not int or body["rejected"] != 0 or \
           type(body.get("dropped")) is not int or body["dropped"] != 0 or \
           not isinstance(body.get("receipt_id"), str) or \
           not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", body["receipt_id"]) or \
           (_header(response_headers, "X-Flare-Receipt-ID") and
            body["receipt_id"] != _header(response_headers, "X-Flare-Receipt-ID")):
            raise CanaryFailure("ingest_acceptance")
        return body["receipt_id"]
    raise CanaryFailure("ingest_acceptance")


def _receipt(client, config, receipt_id, sleep):
    path = "/api/projects/" + config.project_id + "/ingest-receipts?limit=10"
    headers = {"Authorization": "Bearer " + config.viewer_api_key}
    for attempt in range(3):
        try:
            status, _, body = client.json("GET", path, headers)
        except CanaryFailure as exc:
            if attempt == 2:
                raise CanaryFailure("receipt_transport") from exc
            sleep(1)
            continue
        if status != 200 or not isinstance(body, list):
            raise CanaryFailure("receipt_unavailable")
        match = next((row for row in body if isinstance(row, dict) and row.get("id") == receipt_id), None)
        if match is not None:
            if match.get("pillar") == "logs" and match.get("protocol") == "native" and \
               match.get("outcome") == "accepted" and match.get("completed_at") and \
               type(match.get("received_count")) is int and match["received_count"] == 1 and \
               type(match.get("accepted_count")) is int and match["accepted_count"] == 1 and \
               type(match.get("persisted_count")) is int and match["persisted_count"] == 1 and \
               type(match.get("rejected_count")) is int and match["rejected_count"] == 0 and \
               type(match.get("dropped_count")) is int and match["dropped_count"] == 0 and \
               type(match.get("http_status")) is int and match["http_status"] == 200 and \
               match.get("retryable") is False:
                return
            raise CanaryFailure("receipt_invalid")
        if attempt < 2:
            sleep(1)
    raise CanaryFailure("receipt_missing")


def _query(client, config, marker, sleep):
    # Keep the unique marker out of public access-log URLs. A dedicated canary
    # project makes a fixed prefix query sufficient; compare the exact body.
    path = "/api/projects/" + config.project_id + "/logs?" + parse.urlencode({"q": "flare-canary-", "hours": "1", "limit": "5"})
    headers = {"Authorization": "Bearer " + config.viewer_api_key}
    for attempt in range(5):
        try:
            status, _, body = client.json("GET", path, headers)
        except CanaryFailure as exc:
            if attempt == 4:
                raise CanaryFailure("query_transport") from exc
            sleep(1)
            continue
        if status != 200 or not isinstance(body, dict) or not isinstance(body.get("logs"), list):
            raise CanaryFailure("query_unavailable")
        if any(isinstance(row, dict) and row.get("body") == marker for row in body["logs"]):
            return
        if attempt < 4:
            sleep(1)
    raise CanaryFailure("query_missing")


def probe_once(config, client, marker=None, sleep=time.sleep):
    marker = marker or "flare-canary-" + secrets.token_hex(16)
    if not re.fullmatch(r"flare-canary-[0-9a-f]{32}", marker):
        raise CanaryFailure("marker_invalid")
    _ready(client, config)
    receipt_id = _ingest(client, config, marker, sleep)
    _receipt(client, config, receipt_id, sleep)
    _query(client, config, marker, sleep)


def ping_deadman(url, opener=None):
    """Signal success only; keep secret URL out of every failure path."""
    if opener is None:
        opener = request.build_opener(request.ProxyHandler({}),
                                      request.HTTPSHandler(context=ssl.create_default_context()),
                                      NoRedirect())
    success = False
    try:
        with opener.open(request.Request(url, method="GET"),
                         timeout=DEADMAN_TIMEOUT_SECONDS) as response:
            success = type(response.status) is int and 200 <= response.status < 300
    except error.HTTPError as exc:
        exc.close()
    except (error.URLError, TimeoutError, OSError):
        pass
    if not success:
        # Raise outside the handler: even the exception context must not retain
        # a urllib error containing the secret URL.
        raise CanaryFailure("deadman_unavailable")


def send_mail(config, subject, body):
    message = EmailMessage()
    message["From"] = config.from_email
    message["To"] = config.to_email
    message["Subject"] = subject
    message.set_content(body)
    with smtplib.SMTP(SMTP_HOST, SMTP_PORT, timeout=10) as smtp:
        smtp.starttls(context=ssl.create_default_context())
        smtp.login("resend", config.resend_api_key)
        smtp.send_message(message)


def _read_state(path):
    try:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
        if isinstance(value, dict) and value.get("status") in ("ok", "failed"):
            return value
    except (FileNotFoundError, ValueError, OSError):
        pass
    return {}


def _write_state(path, value):
    fd, temp = tempfile.mkstemp(prefix="state-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(value, handle, separators=(",", ":"))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, path)
        dir_fd = os.open(path.parent, os.O_DIRECTORY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def cycle(config, client, state_file, send=send_mail, now=time.time, sleep=time.sleep,
          ping=ping_deadman):
    previous = _read_state(state_file)
    try:
        probe_once(config, client, sleep=sleep)
        if config.deadman_url is not None:
            ping(config.deadman_url)
    except CanaryFailure as exc:
        reason = str(exc)
        last = previous.get("last_alert_at", 0)
        should_alert = previous.get("status") != "failed" or previous.get("reason") != reason or \
            not isinstance(last, (int, float)) or now() - last >= REPEAT_ALERT_SECONDS
        if should_alert:
            send(config, "Flare outside-in canary FAILED", "The independent Flare canary failed: " + reason +
                 "\nCheck public ingress, Flare readiness, retained ingest receipt, and project log query.\n")
        _write_state(state_file, {"status": "failed", "reason": reason,
                                  "last_alert_at": now() if should_alert else last})
        return reason
    if previous.get("status") == "failed":
        send(config, "Flare outside-in canary recovered",
             "The independent Flare readiness, ingest receipt, and query cycle passed.\n")
    _write_state(state_file, {"status": "ok", "reason": "", "last_alert_at": 0})
    return "ok"


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", default="/etc/flare-outside-in-canary.json")
    parser.add_argument("--state-dir", default="/var/lib/flare-outside-in-canary")
    parser.add_argument("--send-test-alert", action="store_true")
    args = parser.parse_args(argv)
    try:
        config = read_config(args.config)
        if args.send_test_alert:
            send_mail(config, "TEST: Flare outside-in canary alert route",
                      "The independent alert route accepted this test message.\n")
            print("CANARY_TEST_ALERT_ACCEPTED")
            return 0
        directory = Path(args.state_dir)
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        with (directory / "lock").open("a+") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = cycle(config, PublicFlareClient(), directory / "state.json")
        print("CANARY_OK" if result == "ok" else "CANARY_FAIL:" + result)
        return 0 if result == "ok" else 1
    except (CanaryFailure, OSError, smtplib.SMTPException, BlockingIOError):
        # Never print exception text: urllib and SMTP exceptions can carry
        # request URLs or credentials. The external deadman covers silence.
        print("CANARY_RUN_OR_ALERT_FAILED", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
