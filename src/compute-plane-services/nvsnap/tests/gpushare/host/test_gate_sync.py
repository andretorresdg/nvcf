#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""The launch gate and GPU drain checks in libnvsnap_gpushare.

No GPU: a probe program includes gpushare.c and calls its gate and sync
functions directly, against a stub driver whose cuCtxSynchronize takes
SYNC_MS and counts its calls. The test checks that

  * a "step" permit a held launch has not taken yet counts as busy, and
    quiesce takes it back, so that launch cannot pass the gate afterwards;
  * a drain check ("poll") uses a synchronization that finished between
    two polls, instead of discarding it and reporting busy forever;
  * quiesce never uses a synchronization that finished, or started, before
    it was called.

Run: python3 -I tests/gpushare/host/test_gate_sync.py
(CUDA_INC names the directory with cuda.h; default /usr/local/cuda/include.)
"""

import os
import shutil
import subprocess
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.normpath(os.path.join(HERE, "../../../docker/agent/gpushare"))
CUDA_INC = os.environ.get("CUDA_INC", "/usr/local/cuda/include")

STUB = r"""
#include <stdlib.h>
#include <unistd.h>
typedef int CUresult;
int stub_syncs;
CUresult cuInit(unsigned f) { (void)f; return 0; }
CUresult cuGetErrorName(CUresult r, const char **s) { (void)r; *s = "stub"; return 0; }
CUresult cuDeviceGetCount(int *n) { *n = 1; return 0; }
CUresult cuDevicePrimaryCtxGetState(int d, unsigned *f, int *a) { (void)d; *f = 0; *a = 1; return 0; }
CUresult cuDevicePrimaryCtxRetain(void **c, int d) { (void)d; *c = (void *)1; return 0; }
CUresult cuDevicePrimaryCtxRelease(int d) { (void)d; return 0; }
CUresult cuCtxPushCurrent(void *c) { (void)c; return 0; }
CUresult cuCtxPopCurrent(void **c) { *c = (void *)1; return 0; }
CUresult cuCtxSynchronize(void)
{
    const char *e = getenv("SYNC_MS");
    usleep((e ? atoi(e) : 0) * 1000);
    __atomic_add_fetch(&stub_syncs, 1, __ATOMIC_SEQ_CST);
    return 0;
}
"""

PROBE = r"""
#include "gpushare.c"

extern int stub_syncs;
static void *waiter(void *arg) { gate_enter(&lgate); gate_exit(&lgate); *(int *)arg = 1; return NULL; }

int main(int argc, char **argv)
{
    CUresult r;
    char rep[256];
    if (argc > 1 && !strcmp(argv[1], "permit")) {
        /* A "step" permit not taken yet (the waiter has not run). */
        lgate.closed = 1;
        lgate.pass = 1;
        printf("drained=%d\n", drained(50, &r));
        do_quiesce(rep, sizeof(rep));
        printf("quiesce=%s pass=%d\n", rep[0] ? rep : "ok", lgate.pass);
        int passed = 0;
        pthread_t t;
        pthread_create(&t, NULL, waiter, &passed);
        usleep(200000);
        printf("passed_after_quiesce=%d\n", passed);
        gate_open();
        pthread_join(t, NULL);
        printf("passed_after_open=%d\n", passed);
        return 0;
    }
    if (argc > 1 && !strcmp(argv[1], "between")) {
        /* Polls 2 ms long, 10 ms apart; each sync takes SYNC_MS (5). */
        lgate.closed = 1;
        int idle = 0;
        for (int i = 0; i < 10; i++) {
            idle += drained(2, &r) == 1;
            usleep(10000);
        }
        printf("idle_polls=%d\n", idle);
        return 0;
    }
    if (argc > 1 && !strcmp(argv[1], "fresh")) {
        /* A poll's sync finishes and is not consumed; quiesce must sync
         * again after closing each gate. */
        lgate.closed = 1;
        drained(0, &r);
        usleep(20000);
        int before = __atomic_load_n(&stub_syncs, __ATOMIC_SEQ_CST);
        do_quiesce(rep, sizeof(rep));
        printf("quiesce=%s syncs=%d\n", rep[0] ? rep : "ok",
               __atomic_load_n(&stub_syncs, __ATOMIC_SEQ_CST) - before);
        return 0;
    }
    return 2;
}
"""


class GateSync(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not shutil.which("gcc") or not os.path.exists(os.path.join(CUDA_INC, "cuda.h")):
            raise unittest.SkipTest("needs gcc and cuda.h (CUDA_INC)")
        cls.tmp = tempfile.mkdtemp()
        for name, text in (("stub.c", STUB), ("probe.c", PROBE)):
            with open(os.path.join(cls.tmp, name), "w") as f:
                f.write(text)
        subprocess.run(["gcc", "-shared", "-fPIC", "-o", os.path.join(cls.tmp, "libcuda.so.1"),
                        os.path.join(cls.tmp, "stub.c")], check=True)
        subprocess.run(["gcc", "-O2", "-I", SRC, "-I", CUDA_INC, "-o", os.path.join(cls.tmp, "probe"),
                        os.path.join(cls.tmp, "probe.c"), os.path.join(cls.tmp, "libcuda.so.1"),
                        "-Wl,-rpath," + cls.tmp, "-ldl", "-lpthread"], check=True)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def probe(self, case, sync_ms=0):
        p = subprocess.run([os.path.join(self.tmp, "probe"), case], capture_output=True, text=True, timeout=60,
                           env=dict(os.environ, SYNC_MS=str(sync_ms)))
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        return dict(kv.split("=", 1) for line in p.stdout.split("\n") for kv in line.split() if "=" in kv)

    def test_untaken_permit_is_busy_and_revoked(self):
        out = self.probe("permit")
        self.assertEqual(out["drained"], "0", out)
        self.assertEqual(out["quiesce"], "ok", out)
        self.assertEqual(out["pass"], "0", out)
        self.assertEqual(out["passed_after_quiesce"], "0", "a launch passed the gate after quiesce")
        self.assertEqual(out["passed_after_open"], "1", out)

    def test_sync_finished_between_polls_is_used(self):
        out = self.probe("between", sync_ms=5)
        self.assertGreater(int(out["idle_polls"]), 0, "every poll reported busy on a drained GPU")

    def test_quiesce_syncs_again(self):
        out = self.probe("fresh", sync_ms=1)
        self.assertEqual(out["quiesce"], "ok", out)
        self.assertGreaterEqual(int(out["syncs"]), 2, out)


if __name__ == "__main__":
    unittest.main(verbosity=2)
