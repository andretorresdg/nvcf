# SPDX-FileCopyrightText: Copyright (c) NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Bootstrap must use the local API while Kubernetes readiness is false."""

import json
import os
from pathlib import Path
import re
import signal
import shlex
import shutil
import subprocess
import tempfile
import time
import unittest

REPO = Path(__file__).resolve().parents[3]
CHART = REPO / "deploy/helm/openbao/helm/scripts"

FAKE_KUBECTL = r'''#!/usr/bin/env python3
import base64, json, os, re, sys, time
from pathlib import Path
p = Path(os.environ['BOOTSTRAP_TEST_STATE'])
s = json.loads(p.read_text())
a = sys.argv[1:]
s['calls'].append(a)
p.write_text(json.dumps(s))
def done(out='', rc=0):
 p.write_text(json.dumps(s)); print(out, end=''); sys.exit(rc)
def stall(operation):
 p.with_suffix('.stall').write_text(operation)
 time.sleep(10)
if any(x.startswith('--request-timeout=') for x in a): done('', 1)
if 'version' in a: done(json.dumps({'serverVersion':{'gitVersion':'v1.36.6'}}))
if 'containerStatuses' in ' '.join(a): done('false')
if 'statefulset' in a:
 if s.get('stall') == 'get': stall('get')
 count = s.get('replicas', 3)
 if s.get('scale_after_snapshot'):
  s['replicas'] = s.pop('scale_after_snapshot')
 done(str(count))
if 'exec' in a and 'status' in a:
 name = next(x for x in a if x.startswith('bao-') and x[4:].isdigit())
 i = int(name.rsplit('-', 1)[1])
 if s.get('unavailable', 0):
  s['unavailable'] -= 1; done('', 1)
 if s.get('stall') == 'exec': stall('exec')
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
if 'exec' in a and 'printenv' in a: done('10.0.0.1')
if 'exec' in a and 'cat' in a:
 if a[a.index('exec')+1] != 'bao-0': done('', 1)
 done('test-service-token')
if 'exec' in a and 'curl' in a:
 if 'Authorization: Bearer test-service-token' not in a: done('', 1)
 done(json.dumps({'keys':[{'kty':'RSA','n':'test','e':'AQAB'}]}))
if 'secret' in a and 'get' in a:
 name = a[a.index('secret') + 1]
 if name.endswith('-root-token') and s.get('root_delay', 0):
  s['root_delay'] -= 1
  if not s['root_delay']: s['secrets'][name] = {'root_token':'test-root'}
 if s.get('secret_read_delay'):
  p.with_suffix('.secret-read').write_text(name)
  time.sleep(s['secret_read_delay'])
 data = s['secrets'].get(name)
 if data is None: done('', 0 if '--ignore-not-found' in a else 1)
 if a[-1] == 'name': done(name)
 version = s.setdefault('secret_versions', {}).setdefault(name, 1)
 encoded = {k:base64.b64encode(v.encode()).decode() for k,v in data.items()}
 if a[-1] == 'json': done(json.dumps({'metadata':{'name':name, 'resourceVersion':str(version)}, 'data':encoded}))
 match = re.fullmatch(r'jsonpath=\{\.data\.([^}]+)\}', a[-1])
 if match: done(encoded.get(match[1], ''))
 done('', 1)
if 'secret' in a and ('patch' in a or 'create' in a):
 s['writes'] += 1
 if s.get('fail_write'): done('', 1)
 name = a[a.index('generic')+1] if 'create' in a else a[a.index('secret')+1]
 failures = s.get('write_failures', {})
 if failures.get(name, 0):
  failures[name] -= 1; done('', 1)
 versions = s.setdefault('secret_versions', {})
 if 'patch' in a:
  patch = json.loads(a[a.index('--patch') + 1])
  if s.get('restore_before_patch') and name.endswith('-unseal'):
   s['restore_before_patch'] = False
   s['secrets'][name]['unseal_key'] = 'restored-key'
   versions[name] = versions.get(name, 1) + 1
  if s.get('restore_root_before_complete') and 'bootstrap_state' in patch['data']:
   s['restore_root_before_complete'] = False
   s['secrets'][name]['root_token'] = 'restored-root'
   versions[name] = versions.get(name, 1) + 1
  if name not in s['secrets']: done('', 1)
  if patch.get('metadata', {}).get('resourceVersion') != str(versions.get(name, 1)): done('Conflict', 1)
  s['secrets'][name].update({k:base64.b64decode(v).decode() for k,v in patch['data'].items()})
 else:
  if name in s['secrets']: done('', 1)
  # Split field=value after the flag, preserving '=' in the literal value.
  s['secrets'][name] = dict(x[len('--from-literal='):].split('=', 1) for x in a if x.startswith('--from-literal='))
  for arg in a:
   if arg.startswith('--from-file='):
    key, filename = arg[len('--from-file='):].split('=', 1)
    s['secrets'][name][key] = Path(filename).read_text()
 versions[name] = versions.get(name, 0) + 1
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
            "secrets": {"bao-unseal": {"unseal_key": ""}}, "calls": [], "writes": 0,
            "init_calls": 0, "unseal_calls": [], "join_calls": [],
        }
        self.state_file = self.path / "state.json"

    def invoke(self, function="initialize_cluster", standalone=False, success=True, api_timeout="5", mode="shamir",
               credential_timeout="120", arguments="test bao", process_timeout=8):
        self.state_file.write_text(json.dumps(self.state))
        source = REPO / "deploy/helm/openbao/deploy.sh" if standalone else CHART / "deploy.sh"
        content = source.read_text()
        body = "" if re.search(r"^" + function + r"\(\)", (CHART / "utils/bootstrap.sh").read_text(), re.M) else re.search(
            r"^" + function + r"\(\) \{.*?^\}", content, re.M | re.S)[0]
        if function == "get_and_save_jwt_signing_key":
            body = body.replace("/tmp/cluster.jwks", str(self.path / "cluster.jwks")).replace("/tmp/jwt.pem", str(self.path / "jwt.pem"))
        helper = CHART / "utils/bootstrap.sh"
        preamble = '\n'.join([
            'log_info() { :; }; log_error() { echo "$*" >&2; }; log_success() { :; }; log_warn() { :; }; log_section() { :; }',
            f'resolve_seal_mode() {{ RESOLVED_SEAL_MODE={mode}; }}',
            'check_sidecar_version() { return 0; }',
            re.search(r"^get_root_token\(\) \{.*?^\}", content, re.M | re.S)[0],
            re.search(r"^get_unseal_key\(\) \{.*?^\}", content, re.M | re.S)[0],
            f'source "{helper}"' if helper.exists() else '',
            'OPENBAO_REPLICAS=3', body, f'{function} {arguments}',
        ])
        env = dict(os.environ, PATH=str(self.path) + os.pathsep + os.environ["PATH"],
                   BOOTSTRAP_TEST_STATE=str(self.state_file), OPENBAO_API_TIMEOUT_SECONDS=api_timeout,
                   OPENBAO_CREDENTIAL_TIMEOUT_SECONDS=credential_timeout)
        process = subprocess.Popen(["bash", "-c", preamble], env=env, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True, start_new_session=True)
        try:
            stdout, stderr = process.communicate(timeout=process_timeout)
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
        self.assertEqual(self.state["secrets"], {"bao-unseal": {"unseal_key": "test-key"}, "bao-root-token": {"root_token": "test-root", "bootstrap_state": "complete"}})
        self.assertFalse(any("containerStatuses" in " ".join(c) for c in self.state["calls"]))

    def test_standalone_initializes_members_without_readiness(self):
        self.invoke(standalone=True)
        self.assertEqual(self.state["init_calls"], 1)
        self.assertEqual(self.state["secrets"], {"bao-unseal": {"unseal_key": "test-key"}, "bao-root-token": {"root_token": "test-root", "bootstrap_state": "complete"}})
        self.assertFalse(any("containerStatuses" in " ".join(c) for c in self.state["calls"]))

    def test_initialized_members_reuse_saved_credentials(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.state["secrets"].update({"bao-root-token": {"root_token": "test-root"}, "bao-unseal": {"unseal_key": "test-key"}})
        self.invoke()
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)

    def test_initialized_without_root_token_fails_without_reinitializing(self):
        self.state["members"][0]["initialized"] = True
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 0)

    def test_uninitialized_with_stale_credentials_fails_closed(self):
        self.state["secrets"]["bao-root-token"] = {"root_token": "old-root"}
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
        self.state["secrets"]["bao-unseal"] = {"unseal_key": "test-key"}
        self.invoke(function="unseal_cluster")
        self.assertEqual(self.state["unseal_calls"], [])
        self.assertEqual(self.state["join_calls"], [])

    def test_partially_joined_cluster_resumes_without_rejoining_existing_member(self):
        self.state["members"][0].update(initialized=True, sealed=False)
        self.state["members"][2]["initialized"] = True
        self.state["secrets"]["bao-unseal"] = {"unseal_key": "test-key"}
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
        self.state["secrets"]["bao-recovery-keys"] = {"recovery_keys_b64": "old-recovery"}
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 0)

    def test_post_install_waits_without_initializing_or_writing_credentials(self):
        self.invoke(function="wait_for_openbao_bootstrap", success=False, api_timeout="1")
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)

    def test_post_install_continues_after_all_members_are_unsealed(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.state["secrets"]["bao-root-token"] = {"root_token": "test-root"}
        self.invoke(function="wait_for_openbao_bootstrap")
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)

    def test_initialized_shamir_without_unseal_key_fails_closed(self):
        self.state["members"][0]["initialized"] = True
        self.state["secrets"]["bao-root-token"] = {"root_token": "test-root"}
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 0)

    def test_initialized_auto_unseal_reuses_root_without_shamir_key(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False, recovery_seal=True)
        self.state["secrets"] = {"bao-root-token": {"root_token": "test-root"}}
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
        self.state["secrets"] = {"bao-recovery-keys": {"recovery_keys_b64": "old-recovery"}}
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
                   BOOTSTRAP_TEST_STATE=str(self.state_file), OPENBAO_API_TIMEOUT_SECONDS="5",
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
        self.state["secrets"].update({"bao-root-token": {"root_token": "test-root"}, "bao-unseal": {"unseal_key": "test-key"}})
        result = self.run_script(standalone=True, OPENBAO_API_TIMEOUT_SECONDS="5")
        self.assertIn("reusing saved bootstrap credentials", result.stdout)
        self.assertIn("Successfully unsealed the cluster", result.stdout)
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)
        self.assertEqual(self.state["join_calls"], ["bao-1", "bao-2"])

    def test_post_upgrade_wait_only_skips_jwt_and_initialization(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.state["secrets"]["bao-root-token"] = {"root_token": "test-root"}
        result = self.run_script(OPENBAO_WAIT_ONLY="true", OPENBAO_API_TIMEOUT_SECONDS="5")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)
        self.assertFalse(any("printenv" in call for call in self.state["calls"]))

    def test_transient_credential_write_failures_are_retried_in_both_scripts(self):
        for standalone in (False, True):
            with self.subTest(standalone=standalone):
                self.state["members"][0]["initialized"] = False
                self.state["secrets"] = {"bao-unseal": {"unseal_key": ""}}
                self.state["write_failures"] = {"bao-unseal": 1, "bao-root-token": 1}
                self.invoke(standalone=standalone)
                self.assertEqual(self.state["secrets"], {"bao-unseal": {"unseal_key": "test-key"}, "bao-root-token": {"root_token": "test-root", "bootstrap_state": "complete"}})
        self.assertEqual(self.state["init_calls"], 2)

    def test_lost_write_responses_are_verified_without_reinitializing(self):
        self.state["lost_write_response"] = True
        self.invoke()
        self.assertEqual(self.state["init_calls"], 1)
        self.assertEqual(self.state["writes"], 3)
        self.assertEqual(self.state["secrets"], {"bao-unseal": {"unseal_key": "test-key"}, "bao-root-token": {"root_token": "test-root", "bootstrap_state": "complete"}})

    def test_root_is_saved_even_if_key_writes_keep_failing(self):
        self.state["write_failures"] = {"bao-unseal": 100}
        self.invoke(success=False)
        self.assertEqual(self.state["init_calls"], 1)
        self.assertEqual(self.state["secrets"]["bao-root-token"]["root_token"], "test-root")

    def test_auto_seal_persists_recovery_keys_and_root(self):
        self.state["secrets"].clear()
        self.invoke(mode="auto")
        self.assertEqual(json.loads(self.state["secrets"]["bao-recovery-keys"]["recovery_keys_b64"]), ["test-recovery"])
        self.assertEqual(self.state["secrets"]["bao-root-token"]["root_token"], "test-root")

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
                self.state_file.with_suffix(".stall").unlink(missing_ok=True)
                started = time.monotonic()
                self.invoke(function="wait_for_openbao_api", success=False, api_timeout="3")
                self.assertEqual(self.state_file.with_suffix(".stall").read_text(), operation)
                self.assertLess(time.monotonic() - started, 6)

    def test_join_failures_are_retried_and_lost_response_is_rechecked(self):
        for lost_response in (False, True):
            with self.subTest(lost_response=lost_response):
                for member in self.state["members"]:
                    member.update(initialized=False, sealed=True)
                self.state["members"][0].update(initialized=True, sealed=False)
                self.state["secrets"]["bao-unseal"] = {"unseal_key": "test-key"}
                self.state["join_calls"] = []
                self.state["join_failures"] = 1
                self.state["lost_join_response"] = lost_response
                self.invoke(function="unseal_cluster")
                self.assertEqual(self.state["join_calls"], ["bao-1", "bao-1", "bao-2"])

    def test_persistent_join_failure_times_out(self):
        self.state["members"][0].update(initialized=True, sealed=False)
        self.state["secrets"]["bao-unseal"] = {"unseal_key": "test-key"}
        self.state["join_failures"] = 1000
        self.invoke(function="unseal_cluster", success=False, api_timeout="1")
        self.assertNotIn("bao-2", self.state["join_calls"])

    def test_wrong_unseal_key_is_rejected(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=True)
        self.state["secrets"]["bao-unseal"] = {"unseal_key": "wrong-key"}
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


    def test_slow_secret_reads_use_a_separate_persistence_budget(self):
        self.state["secret_read_delay"] = 6
        self.invoke(function="persist_bootstrap_secret", api_timeout="1", credential_timeout="20",
                    arguments="test bao-unseal unseal_key test-key patch", process_timeout=25)
        self.assertEqual(self.state["secrets"]["bao-unseal"]["unseal_key"], "test-key")
        self.assertEqual(self.state["writes"], 1)

    def test_credential_persistence_budget_is_bounded(self):
        self.state["secret_read_delay"] = 10
        started = time.monotonic()
        self.invoke(function="persist_bootstrap_secret", credential_timeout="3", success=False,
                    arguments="test bao-unseal unseal_key test-key patch")
        self.assertEqual(self.state_file.with_suffix(".secret-read").read_text(), "bao-unseal")
        self.assertLess(time.monotonic() - started, 6)
        self.assertEqual(self.state["writes"], 0)

    def test_invalid_credential_timeout_fails_before_init(self):
        self.invoke(credential_timeout="0", success=False)
        self.assertEqual(self.state["init_calls"], 0)

    def test_secret_compare_and_swap_preserves_a_concurrent_restore(self):
        self.state["restore_before_patch"] = True
        self.invoke(success=False)
        self.assertEqual(self.state["secrets"]["bao-unseal"]["unseal_key"], "restored-key")
        self.assertEqual(self.state["secrets"]["bao-root-token"]["bootstrap_state"], "pending")
        self.assertEqual(self.state["init_calls"], 1)

    def test_completion_does_not_accept_a_concurrently_changed_root(self):
        self.state["restore_root_before_complete"] = True
        self.invoke(success=False)
        self.assertEqual(self.state["secrets"]["bao-root-token"],
                         {"root_token": "restored-root", "bootstrap_state": "pending"})

    def test_auto_seal_missing_recovery_keys_blocks_retry_and_hook(self):
        self.state["secrets"].clear()
        self.state["write_failures"] = {"bao-recovery-keys": 100}
        self.invoke(mode="auto", success=False)
        self.assertEqual(self.state["secrets"]["bao-root-token"],
                         {"root_token": "test-root", "bootstrap_state": "pending"})
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False, recovery_seal=True)
        self.invoke(mode="auto", success=False)
        self.invoke(function="wait_for_openbao_bootstrap", success=False, api_timeout="3")
        self.assertEqual(self.state["init_calls"], 1)
        self.assertEqual(self.state["secrets"]["bao-root-token"]["bootstrap_state"], "pending")

    def test_pending_credentials_complete_on_retry_and_allow_recovery_export(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False, recovery_seal=True)
        self.state["secrets"] = {
            "bao-root-token": {"root_token": "test-root", "bootstrap_state": "pending"},
            "bao-recovery-keys": {"recovery_keys_b64": '["test-recovery"]'},
        }
        self.invoke(mode="auto")
        self.assertEqual(self.state["secrets"]["bao-root-token"]["bootstrap_state"], "complete")
        del self.state["secrets"]["bao-recovery-keys"]
        self.invoke(mode="auto")
        self.invoke(function="wait_for_openbao_bootstrap")
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 1)

    def test_hook_checks_members_added_after_the_initial_snapshot(self):
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.state["members"][2]["sealed"] = True
        self.state["replicas"] = 2
        self.state["scale_after_snapshot"] = 3
        self.state["secrets"]["bao-root-token"] = {"root_token": "test-root"}
        self.invoke(function="wait_for_openbao_bootstrap", success=False, api_timeout="3")
        self.assertTrue(any("bao-2" in call for call in self.state["calls"]))
        self.state["members"][2]["sealed"] = False
        self.invoke(function="wait_for_openbao_bootstrap")

    def test_fake_preserves_field_names(self):
        self.state["secrets"]["bao-root-token"] = {"rootToken": "test-root"}
        for member in self.state["members"]:
            member.update(initialized=True, sealed=False)
        self.invoke(success=False)
        self.invoke(function="wait_for_openbao_bootstrap", success=False, api_timeout="3")
        self.assertEqual(self.state["init_calls"], 0)

    def test_standalone_jwt_recovery_uses_the_supplied_statefulset(self):
        jwker = self.path / "jwker"
        jwker.write_text("#!/bin/sh\necho test-public-key\n")
        jwker.chmod(0o755)
        self.invoke(function="get_and_save_jwt_signing_key", standalone=True)
        execs = [call for call in self.state["calls"] if "exec" in call]
        self.assertTrue(any("cat" in call for call in execs))
        self.assertTrue(all(call[call.index("exec") + 1] == "bao-0" for call in execs))


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


class BootstrapFeatureAssertionsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="openbao qa ")
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name)
        fake = self.path / "kubectl"
        fake.write_text('''#!/usr/bin/env python3
import json, os, sys
if 'endpointslices' in sys.argv:
 print(os.environ['FEATURE_ENDPOINTS'])
elif 'secrets' in sys.argv:
 print(json.dumps({'items': []}))
else:
 print(os.environ['FEATURE_STATUS'])
''')
        fake.chmod(0o755)
        (self.path / "sealed-member").write_text("openbao-server-0")
        self.feature = (REPO / "tests/bdd/features/openbao-bootstrap-readiness.feature").read_text()

    def run_feature_command(self, match, **inputs):
        line = next(line.strip() for line in self.feature.splitlines()
                    if line.strip().startswith("/bin/bash") and match in line)
        code = shlex.split(line)[5]
        env = dict(os.environ, PATH=str(self.path) + os.pathsep + os.environ["PATH"], **inputs)
        return subprocess.run(["bash", "-e", "-o", "pipefail", "-c", code,
                               "qa", "test-context", "test-namespace", str(self.path)],
                              env=env, capture_output=True, text=True, timeout=5)

    def test_both_cluster_snapshots_reject_missing_or_invalid_ids(self):
        for status in ({}, {"cluster_id": ""}, {"cluster_id": 123}):
            for snapshot in ("cluster-id-before", "cluster-id-after"):
                with self.subTest(status=status, snapshot=snapshot):
                    result = self.run_feature_command(snapshot, FEATURE_STATUS=json.dumps(status))
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn("state=preserved", result.stdout)
        for snapshot in ("cluster-id-before", "cluster-id-after"):
            result = self.run_feature_command(snapshot, FEATURE_STATUS='{"cluster_id":"test-cluster"}')
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_exclusion_requires_another_ready_openbao_member(self):
        def endpoint(name, ready):
            return {"targetRef": {"name": name}, "conditions": {"ready": ready}}
        for endpoints, accepted in [
            ([], False),
            ([endpoint("openbao-server-0", False)], False),
            ([endpoint("openbao-server-0", True), endpoint("openbao-server-1", True)], False),
            ([endpoint("openbao-server-0", False), endpoint("unrelated-pod", True)], False),
            ([endpoint("openbao-server-0", False), endpoint("openbao-server-1", True)], True),
        ]:
            with self.subTest(endpoints=endpoints):
                result = self.run_feature_command("endpoint=excluded", FEATURE_ENDPOINTS=json.dumps({"items": [{"endpoints": endpoints}]}))
                self.assertEqual(result.returncode == 0, accepted, result.stderr)
