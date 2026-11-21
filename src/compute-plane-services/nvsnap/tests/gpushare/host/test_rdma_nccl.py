#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""mx-pause and nixl-pause leave NCCL's RDMA to "release".

A worker can load its weights through ModelExpress (or move KV over NIXL)
and run NCCL over RDMA through libnvsnap_ibv. After the pause, NCCL's
device files are still open: the library closes them at "release", right
after. The pause's check, that no RDMA is left for the dump to fail on,
must not count them, or such a worker can never be captured; anything
else's must still count.

No NIC and no root: the library is built with GPUSHARE_RDMA_DIR naming a
temporary directory, whose files stand for /dev/infiniband's, and with
GPUSHARE_RDMA_EVENT naming a timerfd for a verbs event file. A fake
libnvsnap_ibv opens and maps one device file, and opens an event file, as
NCCL's context would. The test checks that

  * NCCL's open files and mapping are left out of the pause's count;
  * another file open on the same device, another event file, or a
    mapping of another device, still counts;
  * a mapping of NCCL's device whose file another user closed cannot be
    told from NCCL's (the dump fails on it instead);
  * a library without nvsnap_ibv_fds leaves nothing out;
  * end to end, mx-pause passes in a worker whose only RDMA is NCCL's, and
    refuses one with another file open (test_nixl_host.py: nixl-pause).

Run: python3 -I tests/gpushare/host/test_rdma_nccl.py
(CUDA_INC names the directory with cuda.h; default /usr/local/cuda/include.)
"""

import os
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from test_mx_host import CUDA_INC, SRC, STUB  # noqa: E402

# A fake libnvsnap_ibv: at load it opens IBDIR/uverbs0 and maps it, and
# opens an event file (a timerfd), as NCCL's context would; "suspend"
# closes them and "restore" opens them again. -DOLD leaves out
# nvsnap_ibv_fds (a library from before it).
SHIM = r"""
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/timerfd.h>
#include <unistd.h>

static int fd0 = -1, ev0 = -1;
static void *map0;

static void up(void)
{
    char p[512];
    snprintf(p, sizeof(p), "%s/uverbs0", getenv("IBDIR"));
    fd0 = open(p, O_RDWR | O_CLOEXEC);
    map0 = fd0 < 0 ? MAP_FAILED : mmap(NULL, 4096, PROT_READ, MAP_SHARED, fd0, 0);
    ev0 = timerfd_create(CLOCK_MONOTONIC, TFD_CLOEXEC);
}
__attribute__((constructor)) static void init(void) { up(); }

int nvsnap_ibv_fence(char *r, size_t n) { snprintf(r, n, "ok"); return 0; }
int nvsnap_ibv_suspend(char *r, size_t n)
{
    if (map0 != MAP_FAILED) munmap(map0, 4096);
    if (fd0 >= 0) close(fd0);
    if (ev0 >= 0) close(ev0);
    fd0 = ev0 = -1;
    map0 = MAP_FAILED;
    snprintf(r, n, "ok released 1 device(s)");
    return 0;
}
int nvsnap_ibv_restore(char *r, size_t n) { if (fd0 < 0) up(); snprintf(r, n, "ok"); return 0; }
int nvsnap_ibv_map(FILE *fp) { (void)fp; return 0; }
int nvsnap_ibv_connect(const char *p, char *r, size_t n) { (void)p; snprintf(r, n, "ok"); return 0; }
void nvsnap_ibv_open(void) {}
int nvsnap_ibv_count(void) { return 1; }
#ifndef OLD
int nvsnap_ibv_fds(int *fds, int max)
{
    if (fd0 < 0) return 0;
    if (max > 0) fds[0] = fd0;
    if (max > 1) fds[1] = ev0;
    return 2;
}
#endif
"""

# Calls the library's own functions on a process set up as CASE says:
#   owned       NCCL's file and mapping only
#   foreignfd   + another file open on NCCL's device (UCX, say)
#   foreignmap  + a mapping of another device, its file closed
#   samemap     + a mapping of NCCL's device, its file closed
#   foreignev   + another event file
#   noshim      no libnvsnap_ibv; a file open as in "owned"
# then prints the pause's count (pause_open, pause_mapped).
PROBE = r"""
#include "gpushare.c"
#include <sys/timerfd.h>

static void open_map(const char *dev, int keep)
{
    char p[512];
    snprintf(p, sizeof(p), "%s/%s", getenv("IBDIR"), dev);
    int fd = open(p, O_RDWR);
    if (fd < 0 || mmap(NULL, 4096, PROT_READ, MAP_SHARED, fd, 0) == MAP_FAILED) exit(7);
    if (!keep) close(fd);
}

int main(int argc, char **argv)
{
    const char *c = argc > 2 ? argv[1] : "";
    if (strcmp(c, "noshim") && !dlopen(argv[2], RTLD_NOW | RTLD_LOCAL)) { puts(dlerror()); return 3; }
    if (!strcmp(c, "noshim")) {
        char p[512];
        snprintf(p, sizeof(p), "%s/uverbs0", getenv("IBDIR"));
        if (open(p, O_RDWR) < 0) return 4;
    }
    if (!strcmp(c, "foreignfd")) {
        char p[512];
        snprintf(p, sizeof(p), "%s/uverbs0", getenv("IBDIR"));
        if (open(p, O_RDWR) < 0) return 4;
    }
    if (!strcmp(c, "foreignmap")) open_map("uverbs1", 0);
    if (!strcmp(c, "samemap")) open_map("uverbs0", 0);
    if (!strcmp(c, "foreignev") && timerfd_create(CLOCK_MONOTONIC, 0) < 0) return 5;
    int o, m;
    rdma_files(&o, &m, 1);
    printf("pause_open=%d pause_mapped=%d\n", o, m);
    return 0;
}
"""

# A worker for the end-to-end cases: the library preloaded, the fake
# libnvsnap_ibv loaded as NCCL loads it (dlopen, local), a fake
# ModelExpress 0.7 loader and a fake vLLM NIXL connector whose pause frees
# nothing of its own. argv: LOG FOREIGN ("1": also opens another file on
# NCCL's device, as UCX left behind).
WORKER = r"""
import ctypes, enum, os, sys, time, types
LOG, FOREIGN = sys.argv[1], sys.argv[2] == "1"
ctypes.CDLL(os.environ["SHIM"], mode=os.RTLD_LOCAL)
if FOREIGN:
    keep = os.open(os.path.join(os.environ["IBDIR"], "uverbs0"), os.O_RDWR)

class Ctx:
    p2p_enabled = True
    nixl_manager = object()
    mx_client = None

def mod(name, **kw):
    m = types.ModuleType(name)
    m.__dict__.update(kw)
    sys.modules[name] = m
    return m

def pause_serving(ctx):
    ctx.nixl_manager = ctx.mx_client = None

def resume_serving(ctx, model):
    ctx.nixl_manager = object()

mod("modelexpress")
mod("modelexpress.lifecycle", pause_serving=pause_serving, resume_serving=resume_serving)
sys.modules["modelexpress"].lifecycle = sys.modules["modelexpress.lifecycle"]
mod("modelexpress.engines.vllm.loader", _loader_registry={0: types.SimpleNamespace(_ctx=Ctx())},
    _nixl_managers={})

h = ctypes.c_ulonglong()
fd = os.open(os.devnull, os.O_RDONLY)
imp = ctypes.CDLL("libcuda.so.1").cuMemImportFromShareableHandle
r = imp(ctypes.byref(h), ctypes.c_void_p(fd), 1)
print("ready" if r == 0 else "import failed: %d" % r, flush=True)
while True:
    time.sleep(0.1)
"""


def rdma_defines(ib):
    """Build flags: IB's files stand for RDMA devices, a timerfd for a verbs event file."""
    return ["-DGPUSHARE_RDMA_DIR=\"%s/\"" % ib, "-DGPUSHARE_RDMA_EVENT=\"anon_inode:[timerfd]\""]


class RdmaNCCL(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not shutil.which("gcc") or not os.path.exists(os.path.join(CUDA_INC, "cuda.h")):
            raise unittest.SkipTest("needs gcc and cuda.h (CUDA_INC)")
        cls.tmp = tempfile.mkdtemp()
        cls.ib = os.path.join(cls.tmp, "ib")
        os.mkdir(cls.ib)
        for dev in ("uverbs0", "uverbs1"):
            with open(os.path.join(cls.ib, dev), "wb") as f:
                f.write(b"\0" * 4096)
        t = cls.tmp

        def cc(*args):
            subprocess.run(["gcc", "-O2", "-Wall", "-Wextra", "-Werror", "-I", SRC, "-I", CUDA_INC,
                            *rdma_defines(cls.ib), *args], check=True)
        for name, text in (("stub.c", STUB), ("shim.c", SHIM), ("probe.c", PROBE)):
            with open(os.path.join(t, name), "w") as f:
                f.write(text)
        cc("-shared", "-fPIC", "-o", os.path.join(t, "libcuda.so.1"), os.path.join(t, "stub.c"))
        for out, extra in (("libnvsnap_ibv.so", []), ("old/libnvsnap_ibv.so", ["-DOLD"])):
            os.makedirs(os.path.dirname(os.path.join(t, out)), exist_ok=True)
            cc("-shared", "-fPIC", "-Wl,-soname,libnvsnap_ibv.so", *extra, "-o", os.path.join(t, out),
               os.path.join(t, "shim.c"))
        cc("-Wno-unused-function", "-o", os.path.join(t, "probe"), os.path.join(t, "probe.c"),
           os.path.join(t, "libcuda.so.1"), "-Wl,-rpath," + t, "-ldl", "-lpthread")
        cc("-fpatchable-function-entry=4", "-shared", "-fPIC", "-o", os.path.join(t, "libnvsnap_gpushare.so"),
           os.path.join(SRC, "gpushare.c"), "-ldl", "-lpthread")
        cc("-o", os.path.join(t, "nvsnap-gpu-suspend"), os.path.join(SRC, "nvsnap-gpu-suspend.c"), "-ldl", "-lpthread")
        cls.env = dict(os.environ, LD_LIBRARY_PATH=t, IBDIR=cls.ib)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def probe(self, case, shim="libnvsnap_ibv.so", **env):
        p = subprocess.run([os.path.join(self.tmp, "probe"), case, os.path.join(self.tmp, shim)],
                           capture_output=True, text=True, timeout=60, env=dict(self.env, **env))
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        return dict(kv.split("=", 1) for kv in p.stdout.split())

    def test_nccls_own_are_left_to_release(self):
        out = self.probe("owned")
        self.assertEqual((out["pause_open"], out["pause_mapped"]), ("0", "0"), out)

    def test_another_file_on_nccls_device_counts(self):
        out = self.probe("foreignfd")
        self.assertEqual((out["pause_open"], out["pause_mapped"]), ("1", "0"), out)

    def test_another_event_file_counts(self):
        out = self.probe("foreignev")
        self.assertEqual((out["pause_open"], out["pause_mapped"]), ("1", "0"), out)

    def test_a_mapping_of_another_device_counts(self):
        out = self.probe("foreignmap")
        self.assertEqual((out["pause_open"], out["pause_mapped"]), ("0", "1"), out)

    def test_a_mapping_of_nccls_device_without_its_file_is_not_seen(self):
        # rdma-core unmaps at close, so another user leaves none; if one is
        # left, the dump fails on it and the capture rolls back.
        out = self.probe("samemap")
        self.assertEqual((out["pause_open"], out["pause_mapped"]), ("0", "0"), out)

    def test_library_without_fds_leaves_nothing_out(self):
        out = self.probe("owned", shim="old/libnvsnap_ibv.so")
        self.assertEqual((out["pause_open"], out["pause_mapped"]), ("2", "1"), out)

    def test_without_the_library_everything_counts(self):
        out = self.probe("noshim")
        self.assertEqual((out["pause_open"], out["pause_mapped"]), ("1", "0"), out)

    def worker(self, foreign):
        log = os.path.join(self.tmp, "log.%s" % self._testMethodName)
        env = dict(self.env, LD_PRELOAD=os.path.join(self.tmp, "libnvsnap_gpushare.so"),
                   SHIM=os.path.join(self.tmp, "libnvsnap_ibv.so"))
        p = subprocess.Popen([sys.executable, "-I", "-c", WORKER, log, "1" if foreign else "0"], env=env,
                             stdout=subprocess.PIPE, text=True)
        self.addCleanup(p.stdout.close)
        self.addCleanup(p.wait)
        self.addCleanup(p.kill)
        line = p.stdout.readline().strip()
        if line != "ready":
            self.fail("worker did not start: %r" % line)
        return p.pid

    def tool(self, pid, *args):
        return subprocess.run([os.path.join(self.tmp, "nvsnap-gpu-suspend"), *args, str(pid)],
                              env=self.env, capture_output=True, text=True, timeout=60)

    def test_mx_pause_with_nccl_rdma(self):
        pid = self.worker(False)
        p = self.tool(pid, "mx-pause")
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        self.assertIn("ok paused 1 ModelExpress worker(s)", p.stdout)
        self.assertIn("0 RDMA device file(s) open, 0 mapped", p.stdout)
        self.assertEqual(self.tool(pid, "mx-resume").returncode, 0)

    def test_mx_pause_refuses_other_rdma(self):
        pid = self.worker(True)
        p = self.tool(pid, "mx-pause")
        self.assertNotEqual(p.returncode, 0, p.stdout)
        self.assertIn("1 RDMA device file(s) still open, 0 mapped", p.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
