# SPDX-FileCopyrightText: Copyright (c) NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Bootstrap must use the local API while Kubernetes readiness is false."""

import json
import os
from pathlib import Path
import re
import signal
import shutil
import subprocess
import tempfile
import time
import unittest

REPO = Path(__file__).resolve().parents[3]
CHART = REPO / "deploy/helm/openbao/helm/scripts"

FAKE_KUBECTL = r'''#!/usr/bin/env python3
import base64, json, os, sys, time
from pathlib import Path
p = Path(os.environ['BOOTSTRAP_TEST_STATE'])
s = json.loads(p.read_text())
a = sys.argv[1:]
s['calls'].append(a)
def done(out='', rc=0):
 p.write_text(json.dumps(s)); print(out, end=''); sys.exit(rc)
# Explicit request-timeout disables in-cluster fallback in the shipped client.
if any(x.startswith('--request-timeout=') for x in a): done('', 1)
if 'version' in a: done(json.dumps({'serverVersion':{'gitVersion':'v1.36.6'}}))
if 'containerStatuses' in ' '.join(a):
 done('false')
if 'statefulset' in a:
 if s.get('stall') == 'get': time.sleep(10)
 done(str(s.get('replicas', 3)))
if 'exec' in a and 'status' in a:
 name = next(x for x in a if x.startswith('bao-') and x[4:].isdigit())
 i = int(name.rsplit('-', 1)[1])
 if s.get('unavailable', 0):
  s['unavailable'] -= 1; done('', 1)
 if s.get('stall') == 'exec': time.sleep(10)
 status = dict(s['members'][i])
 counts = s.setdefault('status_calls', {})
 counts[str(i)] = counts.get(str(i), 0) + 1
 if s.get('reseal_primary') and i == 1 and counts[str(i)] == 2:
  s['members'][0]['sealed'] = True
  s['members'][1]['sealed'] = False
 if s.get('bad_status'): done('{"initialized":"false","sealed":true}', 2)
 done(json.dumps(status), 2 if status['sealed'] else 0)
if 'exec' in a and 'init' in a:
 s['init_calls'] += 1
 if s.get('deny_init'): done('', 1)
 if s['members'][0]['initialized']: done('', 1)
 s['members'][0]['initialized'] = True
 done(json.dumps({'unseal_keys_b64':['test-key'], 'root_token':'test-root', 'recovery_keys_b64':['test-recovery']}))
if 'secret' in a and 'get' in a:
 name = a[a.index('secret') + 1]
 if name.endswith('-root-token') and s.get('root_delay', 0):
  s['root_delay'] -= 1
  if not s['root_delay']: s['secrets'][name] = 'test-root'
 value = s['secrets'].get(name)
 if value is None:
  done('', 0 if '--ignore-not-found' in a else 1)
 if a[-1] == 'name': done(name)
 done(base64.b64encode(value.encode()).decode())
if 'secret' in a and ('patch' in a or 'create' in a):
 s['writes'] += 1
 if s.get('fail_write'): done('', 1)
 name = next(x for x in a if x.startswith('bao-'))
 failures = s.get('write_failures', {})
 if failures.get(name, 0):
  failures[name] -= 1; done('', 1)
 if 'patch' in a:
  data = json.loads(a[a.index('--patch') + 1])['data']
  value = base64.b64decode(next(iter(data.values()))).decode()
 else:
  value = next(x for x in a if x.startswith('--from-literal=')).split('=', 2)[2]
  if name in s['secrets']: done('', 1)
 s['secrets'][name] = value
 if s.get('lost_write_response'): done('', 1)
 done()
if 'exec' in a and 'unseal' in a:
 name = next(x for x in a if x.startswith('bao-') and x[4:].isdigit())
 if a[-1] != 'test-key': done('invalid unseal key', 1)
 s['members'][int(name.rsplit('-', 1)[1])]['sealed'] = False
 s['unseal_calls'].append(name); done()
if 'exec' in a and 'join' in a:
 name = next(x for x in a if x.startswith('bao-') and x[4:].isdigit())
 s['join_calls'].append(name)
 if s.get('join_failures', 0):
  s['join_failures'] -= 1; done('', 1)
 s['members'][int(name.rsplit('-', 1)[1])]['initialized'] = True
 if s.get('lost_join_response'): done('', 1)
 done()
done('', 1)
'''


class BootstrapReadinessTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name)
        (self.path / "kubectl").write_text(FAKE_KUBECTL)
        (self.path / "kubectl").chmod(0o755)
        (self.path / "sleep").write_text("#!/bin/sh\n/bin/sleep 0.01\n")
        (self.path / "sleep").chmod(0o755)
        self.state = {
            "replicas": 3,
            "members": [dict(initialized=False, sealed=True, recovery_seal=False) for _ in range(3)],
            "secrets": {"bao-unseal": ""}, "calls": [], "writes": 0,
            "init_calls": 0, "unseal_calls": [], "join_calls": [],
        }
        self.state_file = self.path / "state.json"

    def invoke(self, function="initialize_cluster", standalone=False, success=True, api_timeout="5", mode="shamir"):
        self.state_file.write_text(json.dumps(self.state))
        source = REPO / "deploy/helm/openbao/deploy.sh" if standalone else CHART / "deploy.sh"
        content = source.read_text()
        body = "" if function in ("wait_for_openbao_bootstrap", "wait_for_openbao_api") else re.search(
            r"^" + function + r"\(\) \{.*?^\}", content, re.M | re.S)[0]
        helper = CHART / "utils/bootstrap.sh"
        preamble = '\n'.join([
            'log_info() { :; }; log_error() { echo "$*" >&2; }; log_success() { :; }; log_warn() { :; }; log_section() { :; }',
            f'resolve_seal_mode() {{ RESOLVED_SEAL_MODE={mode}; }}',
            'check_sidecar_version() { return 0; }',
            re.search(r"^get_root_token\(\) \{.*?^\}", content, re.M | re.S)[0],
            re.search(r"^get_unseal_key\(\) \{.*?^\}", content, re.M | re.S)[0],
            f'source "{helper}"' if helper.exists() else '',
            'OPENBAO_REPLICAS=3', body, f'{function} test bao',
        ])
        env = dict(os.environ, PATH=str(self.path) + os.pathsep + os.environ["PATH"],
                   BOOTSTRAP_TEST_STATE=str(self.state_file), OPENBAO_API_TIMEOUT_SECONDS=api_timeout)
        process = subprocess.Popen(["bash", "-c", preamble], env=env, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True, start_new_session=True)
        try:
            stdout, stderr = process.communicate(timeout=8)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.communicate()
            self.fail("bootstrap waited for Kubernetes Ready instead of the local API")
        self.state = json.loads(self.state_file.read_text())
        if success:
            self.assertEqual(process.returncode, 0, stderr + stdout)
        else:
            self.assertNotEqual(process.returncode, 0, stdout)
        return stderr

    def test_uninitialized_not_ready_members_can_be_initialized(self):
        self.invoke()
        self.assertEqual(self.state["init_calls"], 1)
        self.assertEqual(self.state["secrets"], {"bao-unseal": "test-key", "bao-root-token": "test-root"})
        self.assertFalse(any("containerStatuses" in " ".join(c) for c in self.state["calls"]))

    def test_standalone_initializes_members_without_readiness(self):
        self.invoke(standalone=True)
        self.assertEqual(self.state["init_calls"], 1)
        self.assertEqual(self.state["secrets"], {"bao-unseal": "test-key", "bao-root-token": "test-root"})
        self.assertFalse(any("containerStatuses" in " ".join(c) for c in self.state["calls"]))

    def test_initialized_members_reuse_saved_credentials(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.state["secrets"].update({"bao-root-token": "test-root", "bao-unseal": "test-key"})
        self.invoke()
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)

    def test_initialized_without_root_token_fails_without_reinitializing(self):
        self.state["members"][0]["initialized"] = True
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 0)

    def test_uninitialized_with_stale_credentials_fails_closed(self):
        self.state["secrets"]["bao-root-token"] = "old-root"
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 0)

    def test_retry_after_failed_key_write_does_not_initialize_twice(self):
        self.state["fail_write"] = True
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 1)
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 1)

    def test_healthy_members_are_not_rejoined_or_unsealed(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.state["secrets"]["bao-unseal"] = "test-key"
        self.invoke(function="unseal_cluster")
        self.assertEqual(self.state["unseal_calls"], [])
        self.assertEqual(self.state["join_calls"], [])

    def test_partially_joined_cluster_resumes_without_rejoining_existing_member(self):
        self.state["members"][0].update(initialized=True, sealed=False)
        self.state["members"][2]["initialized"] = True
        self.state["secrets"]["bao-unseal"] = "test-key"
        self.invoke(function="unseal_cluster")
        self.assertEqual(self.state["join_calls"], ["bao-1"])
        self.assertEqual(self.state["unseal_calls"], ["bao-1", "bao-2"])

    def test_transient_api_failure_is_retried(self):
        self.state["unavailable"] = 1
        self.invoke()
        self.assertEqual(self.state["init_calls"], 1)

    def test_unavailable_api_times_out_without_writes(self):
        self.state["unavailable"] = 10000
        self.invoke(success=False, api_timeout="1")
        self.assertEqual(self.state["writes"], 0)
        self.assertEqual(self.state["init_calls"], 0)

    def test_malformed_status_times_out_without_writes(self):
        self.state["bad_status"] = True
        self.invoke(success=False, api_timeout="1")
        self.assertEqual(self.state["writes"], 0)
        self.assertEqual(self.state["init_calls"], 0)

    def test_live_replica_count_is_used(self):
        self.state["replicas"] = 2
        self.state["members"] = self.state["members"][:2]
        self.invoke()
        self.assertFalse(any("bao-2" in call for call in self.state["calls"]))

    def test_missing_unseal_secret_fails_before_initialization(self):
        self.state["secrets"].clear()
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 0)

    def test_stale_recovery_keys_fail_before_initialization(self):
        self.state["secrets"]["bao-recovery-keys"] = "old-recovery"
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 0)

    def test_post_install_waits_without_initializing_or_writing_credentials(self):
        self.invoke(function="wait_for_openbao_bootstrap", success=False, api_timeout="1")
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)

    def test_post_install_continues_after_all_members_are_unsealed(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.state["secrets"]["bao-root-token"] = "test-root"
        self.invoke(function="wait_for_openbao_bootstrap")
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)

    def test_initialized_shamir_without_unseal_key_fails_closed(self):
        self.state["members"][0]["initialized"] = True
        self.state["secrets"]["bao-root-token"] = "test-root"
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 0)

    def test_initialized_auto_unseal_reuses_root_without_shamir_key(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False, recovery_seal=True)
        self.state["secrets"] = {"bao-root-token": "test-root"}
        self.invoke(mode="auto")
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)

    def test_auto_unseal_wait_does_not_join_or_read_shamir_key(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False, recovery_seal=True)
        self.state["secrets"].clear()
        self.invoke(function="unseal_cluster", mode="auto")
        self.assertEqual(self.state["join_calls"], [])
        self.assertEqual(self.state["unseal_calls"], [])
        self.assertFalse(any("secret" in c for c in self.state["calls"]))

    def test_auto_unseal_wait_times_out_without_manual_unseal(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=True, recovery_seal=True)
        self.invoke(function="unseal_cluster", mode="auto", success=False, api_timeout="1")
        self.assertEqual(self.state["join_calls"], [])
        self.assertEqual(self.state["unseal_calls"], [])

    def test_stale_auto_recovery_credentials_fail_before_initialization(self):
        self.state["secrets"] = {"bao-recovery-keys": "old-recovery"}
        self.invoke(mode="auto", success=False)
        self.assertEqual(self.state["init_calls"], 0)

    def test_invalid_api_timeout_fails_before_calling_kubernetes(self):
        self.invoke(success=False, api_timeout="0")
        self.assertEqual(self.state["calls"], [])

    def run_script(self, standalone=False, **overrides):
        stage = self.path / "stage"
        utils = stage / "utils"
        utils.mkdir(parents=True, exist_ok=True)
        source = REPO / "deploy/helm/openbao" if standalone else CHART
        shutil.copyfile(source / "deploy.sh", stage / "deploy.sh")
        for name in ["utils.sh", "log.sh"]:
            shutil.copyfile(CHART / name, utils / name)
        if standalone:
            utils = stage / "helm/scripts/utils"
            utils.mkdir(parents=True, exist_ok=True)
        for name in ["jwt-plugin.sh", "bootstrap.sh"]:
            shutil.copyfile(CHART / "utils" / name, utils / name)
        jwker = self.path / "jwker"
        jwker.write_text("#!/bin/sh\nexit 0\n")
        jwker.chmod(0o755)
        self.state_file.write_text(json.dumps(self.state))
        env = dict(os.environ, PATH=str(self.path) + os.pathsep + os.environ["PATH"],
                   BOOTSTRAP_TEST_STATE=str(self.state_file), OPENBAO_API_TIMEOUT_SECONDS="1",
                   OPENBAO_BOOTSTRAP_ONLY="false", OPENBAO_WAIT_ONLY="false")
        env.update(overrides)
        result = subprocess.run(["bash", str(stage / "deploy.sh"), "test", "bao", "helm"],
                                env=env, capture_output=True, text=True, timeout=8)
        self.state = json.loads(self.state_file.read_text())
        return result

    def test_post_install_script_never_attempts_initialization(self):
        self.state["deny_init"] = True
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)
        self.assertTrue(any("status" in call for call in self.state["calls"]))
        self.assertIn("Timed out waiting for OpenBao bootstrap to finish", result.stdout + result.stderr)

    def test_standalone_existing_resources_invocation_resumes_bootstrap(self):
        self.state["members"][0].update(initialized=True, sealed=False)
        self.state["secrets"].update({"bao-root-token": "test-root", "bao-unseal": "test-key"})
        result = self.run_script(standalone=True, OPENBAO_API_TIMEOUT_SECONDS="5")
        self.assertIn("reusing saved bootstrap credentials", result.stdout)
        self.assertIn("Successfully unsealed the cluster", result.stdout)
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)
        self.assertEqual(self.state["join_calls"], ["bao-1", "bao-2"])

    def test_post_upgrade_wait_only_skips_jwt_and_initialization(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.state["secrets"]["bao-root-token"] = "test-root"
        result = self.run_script(OPENBAO_WAIT_ONLY="true", OPENBAO_API_TIMEOUT_SECONDS="5")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)
        self.assertFalse(any("printenv" in call for call in self.state["calls"]))

    def test_transient_credential_write_failures_are_retried_in_both_scripts(self):
        for standalone in (False, True):
            with self.subTest(standalone=standalone):
                self.state["members"][0]["initialized"] = False
                self.state["secrets"] = {"bao-unseal": ""}
                self.state["write_failures"] = {"bao-unseal": 1, "bao-root-token": 1}
                self.invoke(standalone=standalone)
                self.assertEqual(self.state["secrets"], {"bao-unseal": "test-key", "bao-root-token": "test-root"})
        self.assertEqual(self.state["init_calls"], 2)

    def test_lost_write_responses_are_verified_without_reinitializing(self):
        self.state["lost_write_response"] = True
        self.invoke()
        self.assertEqual(self.state["init_calls"], 1)
        self.assertEqual(self.state["writes"], 2)
        self.assertEqual(self.state["secrets"], {"bao-unseal": "test-key", "bao-root-token": "test-root"})

    def test_root_is_saved_even_if_key_writes_keep_failing(self):
        self.state["write_failures"] = {"bao-unseal": 100}
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 1)
        self.assertEqual(self.state["secrets"]["bao-root-token"], "test-root")

    def test_auto_seal_persists_recovery_keys_and_root(self):
        self.state["secrets"].clear()
        self.invoke(mode="auto")
        self.assertEqual(json.loads(self.state["secrets"]["bao-recovery-keys"]), ["test-recovery"])
        self.assertEqual(self.state["secrets"]["bao-root-token"], "test-root")

    def test_post_install_waits_for_saved_root_token(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.invoke(function="wait_for_openbao_bootstrap", success=False, api_timeout="1")
        self.state["root_delay"] = 2
        self.invoke(function="wait_for_openbao_bootstrap")
        self.assertEqual(self.state["root_delay"], 0)
        self.assertEqual(self.state["writes"], 0)

    def test_stalled_kubernetes_get_and_exec_are_bounded(self):
        for operation in ("get", "exec"):
            with self.subTest(operation=operation):
                self.state["stall"] = operation
                started = time.monotonic()
                self.invoke(function="wait_for_openbao_api", success=False, api_timeout="1")
                self.assertLess(time.monotonic() - started, 4)

    def test_join_failures_are_retried_and_lost_response_is_rechecked(self):
        for lost_response in (False, True):
            with self.subTest(lost_response=lost_response):
                for member in self.state["members"]:
                    member.update(initialized=False, sealed=True)
                self.state["members"][0].update(initialized=True, sealed=False)
                self.state["secrets"]["bao-unseal"] = "test-key"
                self.state["join_calls"] = []
                self.state["join_failures"] = 1
                self.state["lost_join_response"] = lost_response
                self.invoke(function="unseal_cluster")
                self.assertEqual(self.state["join_calls"], ["bao-1", "bao-1", "bao-2"])

    def test_persistent_join_failure_times_out(self):
        self.state["members"][0].update(initialized=True, sealed=False)
        self.state["secrets"]["bao-unseal"] = "test-key"
        self.state["join_failures"] = 1000
        self.invoke(function="unseal_cluster", success=False, api_timeout="1")
        self.assertNotIn("bao-2", self.state["join_calls"])

    def test_wrong_unseal_key_is_rejected(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=True)
        self.state["secrets"]["bao-unseal"] = "wrong-key"
        self.invoke(function="unseal_cluster", success=False, api_timeout="5")
        self.assertTrue(any("unseal" in call for call in self.state["calls"]))
        self.assertTrue(all(member["sealed"] for member in self.state["members"]))

    def test_auto_seal_rechecks_primary_while_waiting_for_peer(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.state["members"][1]["sealed"] = True
        self.state["reseal_primary"] = True
        self.invoke(function="unseal_cluster", mode="auto", success=False, api_timeout="5")
        self.assertTrue(self.state["members"][0]["sealed"])


class UnsealMonitorTest(unittest.TestCase):
    def test_monitor_retries_lost_process_and_confirms_stopped_state(self):
        for never_stops in (False, True):
            with self.subTest(never_stops=never_stops), tempfile.TemporaryDirectory() as tmp:
                path = Path(tmp)
                for pid in (101, 102):
                    proc = path / "proc" / str(pid)
                    proc.mkdir(parents=True)
                    (proc / "comm").write_text("sleep\n")
                    (proc / "status").write_text("PPid: 1\nState: S\n")
                fake = path / "kubectl"
                fake.write_text(r'''#!/usr/bin/env python3
import os, subprocess, sys
from pathlib import Path
root = Path(os.environ['MONITOR_TEST_DIR'])
a = sys.argv[1:]
script = a[a.index('--') + 3].replace('/proc/', str(root / 'proc') + '/')
prefix = r"""
kill() {
    if test "$1" = -STOP; then
        printf '%s\n' "$2" >> "$MONITOR_TEST_DIR/signals"
        if test "$2" = 101; then return 1; fi
        if test "$MONITOR_NEVER_STOPS" = false; then
            printf 'PPid: 1\nState: T\n' > "$MONITOR_TEST_DIR/proc/$2/status"
        fi
    fi
}
sleep() {
    if test "$1" = 90; then : > "$MONITOR_TEST_DIR/watchdog"; fi
}
"""
sys.exit(subprocess.run(['sh', '-ec', prefix + script, 'qa-monitor', a[-1]]).returncode)
''')
                fake.chmod(0o755)
                env = dict(os.environ, PATH=str(path) + os.pathsep + os.environ["PATH"],
                           MONITOR_TEST_DIR=tmp, MONITOR_NEVER_STOPS=str(never_stops).lower())
                result = subprocess.run(["bash", str(REPO / "tests/bdd/scripts/openbao-unseal-monitor.sh"),
                                         "test-context", "test-namespace", "test-pod", "pause"],
                                        env=env, capture_output=True, text=True, timeout=5)
                signals = (path / "signals").read_text().splitlines()
                self.assertEqual(signals[:2], ["101", "102"])
                if never_stops:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn("monitor=pause", result.stdout)
                    self.assertFalse((path / "watchdog").exists())
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("monitor=pause", result.stdout)
