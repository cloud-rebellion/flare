#!/usr/bin/env python3
"""Run the unchanged product gates and verify the actual PostgreSQL test coverage.

The public checkout has no sibling applications. Its existing estate-wiring
nonexecution is reported separately; no private wiring override is enabled.
"""

import argparse
import json
import os
import pathlib
import re
import selectors
import signal
import subprocess
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
MAX_LOG_BYTES = 64 * 1024 * 1024
EXPECTED_EXTERNAL_SKIP = "first-party browser bridge wiring"
STARTING_CHILD = False
PENDING_SIGNAL = None
REQUIRED = {
    "github.com/bright-interaction/flare/internal/oidc": {
        "TestAssuranceMetadataDoesNotChangeGenericDiscovery",
        "TestGenericPublicIssuerPreservesCustomPortAndEscapedRealm",
        "TestOIDCOperatorPinIsExactAndOptIn",
        "TestOIDCOutboundCallsRecordBoundedIntegrationMetrics",
        "TestOIDCReceiptFailureDoesNotRepeatAuthenticationCalls",
        "TestOIDCResponseLimitAndProviderErrorRedaction",
        "TestVerifiedDiscoveryRejectsIssuerOrJWKSDrift",
        "TestVerifiedOIDCAssuranceBeforeUserinfo",
        "TestVerifiedOIDCProtocol",
    },
    "github.com/bright-interaction/flare/internal/config": {
        "TestColdRetentionEnvDefault",
        "TestConnectorSyncSchedulerIsOptInAndBoundedCadenceLoads",
        "TestHubSpotOAuthRequiresPairedCredentialsAndPublicHTTPS",
        "TestMCPAllowedOriginsLoadBoundsAndNormalizesExplicitAliases",
        "TestMCPOriginDevelopmentLoopbackAndDistinctAuthorities",
        "TestOIDCAssuranceDefaultsAndInvalidSelectionKeepNativeStartup",
        "TestOIDCOperatorPolicyDefaultsAndExplicitPilot",
        "TestProductionLoadsWithAllSecrets",
        "TestProductionRequiresFlareSecretKey",
        "TestProductionRequiresSessionAndCSRF",
        "TestSignupIsClosedByDefault",
        "TestSignupOptInSpellings",
    },
    "github.com/bright-interaction/flare/internal/api": {
        "TestNativeSessionDatabaseFailureIsUnknownAndPreservesSession",
        "TestNativeSessionMissingIdentityIsActuallySignedOut",
        "TestOIDCAssuranceIncompleteSelectedProfileDoesNotAffectOthers",
        "TestOIDCAssuranceSelectsOnlyExactOrganizationAndPreservesLegacyHash",
        "TestOIDCGenericVerifiedEmailAndCrossOrgControlsDB",
        "TestOIDCPrelinkedIdentityPreservesAccountAndRotatesSessionDB",
        "TestOIDCTransactionsConsumeOnceAndRejectConfigurationSwitchDB",
        "TestOIDCTxClearExpiresTheCookie",
        "TestOIDCTxCookieAttributes",
        "TestOIDCTxRejectsExpiredOrIncompleteProof",
        "TestOIDCTxRejectsForeignKey",
        "TestOIDCTxRejectsTampering",
        "TestOIDCTxRoundTrip",
        "TestOIDCTxSelectedAssuranceContextRoundTripsAndRejectsMalformedBinding",
        "TestPortalNativeCrossTabSessionWinsAndRevocationRemainsLocalDB",
        "TestPortalReadinessChoosesOnlyConfiguredPrimaryKeyDB",
        "TestPortalSelectedContextChangesRefuseBeforeAccountEffectsDB",
        "TestPortalUnrelatedOrganizationKeepsLegacyTransactionAndPrelinkRuleDB",
    },
    "github.com/bright-interaction/flare/internal/oidcprofile": {
        "TestProfileConfigurationIsExplicitAndCanonical",
        "TestSignedJSONUnicodeIsNotRepairedIntoEvidence",
        "TestStrictSignedAssuranceProfile",
    },
}
DB_TESTS = {
    name for name in REQUIRED["github.com/bright-interaction/flare/internal/api"]
    if name.endswith("DB")
}


def verify_log(text):
    """Parse standard verbose Go output from the one unchanged legacy test step."""
    in_tests = False
    test_sections = 0
    closed = False
    pending = set()
    passed = {}
    skips = []
    failures = []
    external = []
    for line in text.splitlines():
        if line.startswith("=== ") and line.endswith(" ==="):
            if in_tests:
                closed = True
            in_tests = line == "=== test (RUN, not just compile) ==="
            if in_tests:
                test_sections += 1
            continue
        if line.startswith("SKIPPED: "):
            external.append(line.removeprefix("SKIPPED: "))
        if not in_tests:
            continue
        if re.match(r"^\s*--- SKIP:", line):
            skips.append(line.strip())
        if re.match(r"^\s*--- FAIL:", line) or line.startswith("FAIL"):
            failures.append(line.strip())
        match = re.match(r"^--- PASS: ([A-Za-z0-9_]+) \(", line)
        if match:
            pending.add(match.group(1))
        package = re.match(r"^ok\s+(\S+)\s+", line)
        if package:
            name = package.group(1)
            if name in passed:
                raise RuntimeError("duplicate Go package result")
            passed[name] = pending
            pending = set()
    if test_sections != 1 or not closed or pending:
        raise RuntimeError("complete legacy Go test section was not observed")
    if skips or failures:
        raise RuntimeError("actual Go test skips or failures were observed")
    if external != [EXPECTED_EXTERNAL_SKIP]:
        raise RuntimeError("unexpected missing legacy gate or external-wiring result")
    missing = {
        package: sorted(names - passed.get(package, set()))
        for package, names in REQUIRED.items()
        if names - passed.get(package, set())
    }
    if missing:
        raise RuntimeError("mandatory authentication/database tests missing: " + json.dumps(missing))
    api = passed.get("github.com/bright-interaction/flare/internal/api", set())
    selected = {name for name in api if re.match(r"^Test(OIDC|Portal|NativeSession)", name)}
    if selected != REQUIRED["github.com/bright-interaction/flare/internal/api"]:
        raise RuntimeError("the exact 18-test API authentication inventory changed")
    if len(DB_TESTS) != 7 or not DB_TESTS <= selected:
        raise RuntimeError("the seven real-database authentication groups did not pass")
    return {
        "status": "passed",
        "go_packages": len(passed),
        "top_level_test_passes": sum(len(names) for names in passed.values()),
        "required_top_level_tests": sum(len(names) for names in REQUIRED.values()),
        "api_authentication_tests": len(selected),
        "real_database_authentication_groups": len(DB_TESTS),
        "actual_go_test_skips": 0,
        "actual_go_test_failures": 0,
        "external_gate_not_executed": EXPECTED_EXTERNAL_SKIP,
        "boundary": "product checks only; estate sibling wiring is not in this public repository",
    }


def stop_owned(process):
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        process.wait(timeout=10)
        return
    try:
        process.wait(timeout=15)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait(timeout=10)


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
    if (ROOT / "docker-compose.yml").exists():
        raise RuntimeError("estate compose must not be present in a public checkout")
    if (ROOT / "flare").exists():
        raise RuntimeError("run from the public-only exported tree, without nested private-layout leftovers")
    if os.environ.get("FLARE_CI_WIRING_RUNS_IN_AUTOMATIONS"):
        raise RuntimeError("private wiring overrides are not public qualification")
    for key in ("FLARE_TEST_DATABASE_URL", "TEST_DATABASE_URL"):
        if not os.environ.get(key):
            raise RuntimeError("both disposable PostgreSQL test variables are required")
    if subprocess.check_output(["go", "version"], text=True).split()[:3] != [
        "go", "version", "go1.26.11"
    ]:
        raise RuntimeError("Go 1.26.11 is required")
    if subprocess.check_output(["bun", "--version"], text=True).strip() != "1.3.9":
        raise RuntimeError("Bun 1.3.9 is required")
    output = args.output_dir.resolve()
    if output == ROOT or ROOT in output.parents:
        raise RuntimeError("qualification output must be outside authored source")
    output.mkdir(parents=True, exist_ok=True)
    log = output / "legacy-ci.log"
    env = dict(os.environ)
    # Verbosity changes no gate or test selection. It exposes actual skip/pass
    # markers from the legacy script's single full-suite go test invocation.
    env["GOFLAGS"] = "-mod=readonly -v"
    process = None
    deadline = time.monotonic() + 45 * 60
    size = 0
    try:
        STARTING_CHILD = True
        try:
            process = subprocess.Popen(
                ["bash", "scripts/ci.sh"], cwd=ROOT, env=env,
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True,
            )
        finally:
            STARTING_CHILD = False
        # Defer only the parent's Python exception during spawn. Blocking the
        # OS signal mask here would also block cancellation in the exec child.
        if PENDING_SIGNAL is not None:
            raise SystemExit(128 + PENDING_SIGNAL)
        with log.open("xb") as stream:
            assert process.stdout is not None
            os.set_blocking(process.stdout.fileno(), False)
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                while selector.get_map():
                    if time.monotonic() > deadline:
                        raise RuntimeError("legacy qualification exceeded its deadline")
                    for key, _ in selector.select(timeout=1):
                        chunk = os.read(key.fd, 65536)
                        if not chunk:
                            selector.unregister(key.fileobj)
                            continue
                        size += len(chunk)
                        if size > MAX_LOG_BYTES:
                            raise RuntimeError("legacy qualification exceeded its log bound")
                        stream.write(chunk)
                        sys.stdout.buffer.write(chunk)
                        sys.stdout.buffer.flush()
        code = process.wait(timeout=max(1, deadline - time.monotonic()))
        if code != 0:
            raise RuntimeError("an unchanged legacy product gate failed")
        receipt = verify_log(log.read_text())
        (output / "qualification.json").write_text(json.dumps(receipt, indent=2) + "\n")
        print(json.dumps(receipt), flush=True)
        summary = os.environ.get("GITHUB_STEP_SUMMARY")
        if summary:
            with open(summary, "a") as stream:
                stream.write(
                    "Product qualification passed: 42 mandatory top-level tests, "
                    "18 authentication API tests and seven real PostgreSQL groups; "
                    "zero actual Go test skips/failures. "
                    "Estate-wide sibling wiring is outside this public repository "
                    "and remains explicitly unexecuted.\n"
                )
    finally:
        if process is not None:
            stop_owned(process)
            if process.stdout is not None:
                process.stdout.close()


if __name__ == "__main__":
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    main()
