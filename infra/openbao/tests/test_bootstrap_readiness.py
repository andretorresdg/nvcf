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
import unittest

REPO = Path(__file__).resolve().parents[3]
CHART = REPO / "deploy/helm/openbao/helm/scripts"

FAKE_KUBECTL = r'''#!/usr/bin/env python3
import base64, json, os, sys
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
 done(str(s.get('replicas', 3)))
if 'exec' in a and 'status' in a:
 name = next(x for x in a if x.startswith('bao-') and x[4:].isdigit())
 i = int(name.rsplit('-', 1)[1])
 if s.get('unavailable', 0):
  s['unavailable'] -= 1; done('', 1)
 status = s['members'][i]
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
 value = s['secrets'].get(name)
 if value is None:
  done('', 0 if '--ignore-not-found' in a else 1)
 if 'root-token' in name: done(base64.b64encode(value.encode()).decode())
 if 'unseal' in name: done(base64.b64encode(value.encode()).decode())
 done(name)
if 'secret' in a and ('patch' in a or 'create' in a):
 s['writes'] += 1
 if s.get('fail_write'): done('', 1)
 name = next(x for x in a if x.startswith('bao-'))
 s['secrets'][name] = 'test-key' if name.endswith('-unseal') else 'test-root'
 done()
if 'exec' in a and 'unseal' in a:
 name = next(x for x in a if x.startswith('bao-') and x[4:].isdigit())
 s['members'][int(name.rsplit('-', 1)[1])]['sealed'] = False
 s['unseal_calls'].append(name); done()
if 'exec' in a and 'join' in a:
 name = next(x for x in a if x.startswith('bao-') and x[4:].isdigit())
 s['members'][int(name.rsplit('-', 1)[1])]['initialized'] = True
 s['join_calls'].append(name); done()
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
        body = "" if function == "wait_for_openbao_bootstrap" else re.search(
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
        self.assertFalse(any("containerStatuses" in " ".join(c) for c in self.state["calls"]))

    def test_standalone_initializes_members_without_readiness(self):
        self.invoke(standalone=True)
        self.assertEqual(self.state["init_calls"], 1)
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

    def test_post_install_script_never_attempts_initialization(self):
        stage = self.path / "stage"
        utils = stage / "utils"
        utils.mkdir(parents=True)
        shutil.copyfile(CHART / "deploy.sh", stage / "deploy.sh")
        for name in ["utils.sh", "log.sh"]:
            shutil.copyfile(CHART / name, utils / name)
        for name in ["jwt-plugin.sh", "bootstrap.sh"]:
            shutil.copyfile(CHART / "utils" / name, utils / name)
        jwker = self.path / "jwker"
        jwker.write_text("#!/bin/sh\nexit 0\n")
        jwker.chmod(0o755)
        self.state["deny_init"] = True
        self.state_file.write_text(json.dumps(self.state))
        env = dict(os.environ, PATH=str(self.path) + os.pathsep + os.environ["PATH"],
                   BOOTSTRAP_TEST_STATE=str(self.state_file), OPENBAO_API_TIMEOUT_SECONDS="1",
                   OPENBAO_BOOTSTRAP_ONLY="false")
        result = subprocess.run(["bash", str(stage / "deploy.sh"), "test", "bao", "helm"],
                                env=env, capture_output=True, text=True, timeout=8)
        self.state = json.loads(self.state_file.read_text())
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(self.state["init_calls"], 0)
        self.assertEqual(self.state["writes"], 0)
