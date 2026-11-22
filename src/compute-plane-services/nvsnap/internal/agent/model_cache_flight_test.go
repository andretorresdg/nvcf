// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/modelcache"
	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/webhook"
)

// Single flight across nodes (docs/design/model-cache-single-flight.md),
// in process: every node's controller shares one fake API server and
// serves its routes over httptest.

// flightPod is a cold pod admitted with single flight: the helper's plan
// flags, the pod-uid variable and the signal volume. Its init is running.
func flightPod(t *testing.T, name, node string, uid types.UID) *corev1.Pod {
	t.Helper()
	pod := cachePod(t, name, node, uid)
	init := &pod.Spec.InitContainers[0]
	var cmd []string
	for _, a := range init.Command {
		if a == "--" && len(cmd) > 0 && cmd[len(cmd)-1] != "--" && !hasArg(cmd, "--plan-wait") {
			cmd = append(cmd, "--plan-wait", "60", "--signal", webhook.ModelCacheSignalMount)
		}
		cmd = append(cmd, a)
	}
	init.Command = cmd
	init.Env = append(init.Env, corev1.EnvVar{Name: webhook.PodUIDEnv, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}})
	init.VolumeMounts = append(init.VolumeMounts, corev1.VolumeMount{Name: webhook.ModelCacheSignalVolume, MountPath: webhook.ModelCacheSignalMount})
	limit := resource.MustParse("1Mi")
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: webhook.ModelCacheSignalVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &limit}}})
	d, ok := webhook.ResolveModelCacheDownload(context.Background(), nil, pod, 0)
	if !ok || d.Key != pod.Labels[webhook.ModelCacheKeyLabel] {
		t.Fatalf("single-flight fixture resolves to key %q ok=%v; labelled %q", d.Key, ok, pod.Labels[webhook.ModelCacheKeyLabel])
	}
	pod.Status.Phase = corev1.PodPending
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "download-ngc-model",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}
	return pod
}

func hasArg(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func newFlightNode(t *testing.T, kube *fake.Clientset, node string, urls map[string]string) *mcNode {
	t.Helper()
	n := newMCNode(t, kube, node, urls)
	n.c.singleFlight = true
	n.c.timing = streamTiming{settle: 100 * time.Millisecond, poll: 50 * time.Millisecond, stall: 10 * time.Second}
	n.c.leaseDuration = 2 * time.Second
	n.c.client.Timeout = 5 * time.Second
	n.c.addLiveRoutes(n.router)
	return n
}

func (n *mcNode) volume(pod *corev1.Pod, vol string) string {
	return filepath.Join(n.c.podsRoot, string(pod.UID), "volumes", "kubernetes.io~empty-dir", vol)
}

func (n *mcNode) plan(pod *corev1.Pod) string {
	return modelcache.ReadPlan(n.c.store.KeyDir(keyOf(pod)), string(pod.UID))
}

func (n *mcNode) marker(pod *corev1.Pod, name string) bool {
	_, err := os.Stat(filepath.Join(n.c.store.KeyDir(keyOf(pod)), name))
	return err == nil
}

// signal is the helper's word that its download is over.
func (n *mcNode) signal(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	ents, err := modelcache.Scan(n.volume(pod, "ngc-models"), 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := n.volume(pod, webhook.ModelCacheSignalVolume)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, modelcache.SignalFile), []byte(modelcache.Fingerprint(ents)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func emptyVolumes(t *testing.T, nodes map[*mcNode]*corev1.Pod) {
	t.Helper()
	for n, p := range nodes {
		if err := os.MkdirAll(n.volume(p, "ngc-models"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func tickAll(nodes ...*mcNode) {
	for _, n := range nodes {
		n.c.tick(context.Background())
	}
}

func TestSingleFlightOnePodDownloadsAndTheOthersStreamItsTree(t *testing.T) {
	kube := fake.NewSimpleClientset()
	urls := map[string]string{}
	a, b, c := newFlightNode(t, kube, "a", urls), newFlightNode(t, kube, "b", urls), newFlightNode(t, kube, "c", urls)
	pa, pb, pc := flightPod(t, "pa", "a", "ua"), flightPod(t, "pb", "b", "ub"), flightPod(t, "pc", "c", "uc")
	for _, p := range []*corev1.Pod{pa, pb, pc} {
		addPod(t, kube, p)
	}
	emptyVolumes(t, map[*mcNode]*corev1.Pod{a: pa, b: pb, c: pc})
	tickAll(a, b, c)

	// The first to try holds the Lease and downloads; the others wait.
	if got := []string{a.plan(pa), b.plan(pb), c.plan(pc)}; got[0] != modelcache.PlanDownload || got[1] != modelcache.PlanWait || got[2] != modelcache.PlanWait {
		t.Fatalf("plans %q; want download, wait, wait", got)
	}
	for _, n := range []*mcNode{b, c} {
		if !n.marker(pa, modelcache.StreamingFile) {
			t.Fatal("a waiting node keeps no streaming heartbeat")
		}
	}

	// a's "download" lands files over time; b and c stream them.
	files := map[string]string{"model/config.json": "{}", "model/w-1.bin": "weights one", "model/w-2.bin": "weights two"}
	a.downloadedLive(t, pa, map[string]string{"model/config.json": "{}", "model/w-1.bin": "weights one"})
	for i := 0; i < 10; i++ {
		tickAll(a, b, c)
		time.Sleep(50 * time.Millisecond)
	}
	a.downloadedLive(t, pa, map[string]string{"model/w-2.bin": "weights two"})
	a.signal(t, pa)
	mcEventually(t, "a's final manifest", func() bool { tickAll(a); return a.marker(pa, modelcache.FinalFile(string(pa.UID))) })
	for _, x := range []struct {
		n *mcNode
		p *corev1.Pod
	}{{b, pb}, {c, pc}} {
		mcEventually(t, "done on "+x.n.c.node, func() bool {
			tickAll(x.n)
			return x.n.marker(x.p, modelcache.DoneFile(string(x.p.UID)))
		})
		assertTree(t, x.n.volume(x.p, "ngc-models"), files)
	}
}

// downloadedLive writes files into the pod's volume as a running download
// does: no termination message yet.
func (n *mcNode) downloadedLive(t *testing.T, pod *corev1.Pod, files map[string]string) {
	t.Helper()
	for p, b := range files {
		write(t, n.volume(pod, "ngc-models"), p, b)
	}
}

// A pod whose init is not running yet (an image pull, an earlier init)
// takes no Lease: a holder that cannot start would hold everyone.
func TestSingleFlightElectsOnlyARunningInit(t *testing.T) {
	kube := fake.NewSimpleClientset()
	a := newFlightNode(t, kube, "a", map[string]string{})
	p := flightPod(t, "pa", "a", "ua")
	p.Status.InitContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	addPod(t, kube, p)
	emptyVolumes(t, map[*mcNode]*corev1.Pod{a: p})
	a.c.tick(context.Background())
	if _, err := kube.CoordinationV1().Leases(mcNS).Get(context.Background(), leaseName(keyOf(p)), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a Lease for a pod whose init is not running (err %v)", err)
	}
	if a.plan(p) != "" {
		t.Fatal("planned a pod whose init is not running")
	}
}

// A pod whose volume is not empty is downloading already (its helper gave
// up waiting for a plan): it gets no plan, and nothing streams into it.
func TestSingleFlightLeavesADownloadingVolumeAlone(t *testing.T) {
	kube := fake.NewSimpleClientset()
	a := newFlightNode(t, kube, "a", map[string]string{})
	p := flightPod(t, "pa", "a", "ua")
	addPod(t, kube, p)
	write(t, a.volume(p, "ngc-models"), "partial.bin", "half")
	a.c.tick(context.Background())
	if a.plan(p) != "" {
		t.Fatalf("plan %q for a volume a download writes", a.plan(p))
	}
}

// Expiry is judged by how long this agent has seen the renew time stand
// still, not by comparing clocks across nodes.
func TestSingleFlightTakesOverALeaseOnlyAfterSeeingItStandStill(t *testing.T) {
	kube := fake.NewSimpleClientset()
	b := newFlightNode(t, kube, "b", map[string]string{})
	key := "0123456789abcdef0123456789abcdef"
	dur := int32(2)
	old := metav1.NewMicroTime(time.Now().Add(-time.Hour)) // far behind on b's clock
	holder := "a/ua"
	if _, err := kube.CoordinationV1().Leases(mcNS).Create(context.Background(), &coordv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: leaseName(key), Namespace: mcNS},
		Spec:       coordv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &dur, RenewTime: &old}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, by, err := b.c.acquire(context.Background(), key, "b/ub"); err != nil || got || by != holder {
		t.Fatalf("first look: got=%v by=%q err=%v; want held by a", got, by, err)
	}
	// Its renew time is an hour old on b's clock, but b has only just seen it.
	if got, _, _ := b.c.acquire(context.Background(), key, "b/ub"); got {
		t.Fatal("took over a Lease it had seen for no time")
	}
	time.Sleep(2500 * time.Millisecond)
	if got, _, err := b.c.acquire(context.Background(), key, "b/ub"); err != nil || !got {
		t.Fatalf("after the renew time stood still past the duration: got=%v err=%v", got, err)
	}
}

func TestSingleFlightReleaseDoesNotDeleteASuccessorsLease(t *testing.T) {
	kube := fake.NewSimpleClientset()
	a := newFlightNode(t, kube, "a", map[string]string{})
	key := "0123456789abcdef0123456789abcdef"
	if got, _, err := a.c.acquire(context.Background(), key, "a/ua"); err != nil || !got {
		t.Fatalf("acquire: %v %v", got, err)
	}
	// Taken over while a was away.
	l, _ := kube.CoordinationV1().Leases(mcNS).Get(context.Background(), leaseName(key), metav1.GetOptions{})
	other := "b/ub"
	l.Spec.HolderIdentity = &other
	if _, err := kube.CoordinationV1().Leases(mcNS).Update(context.Background(), l, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	a.c.release(context.Background(), key, "a/ua")
	if l, err := kube.CoordinationV1().Leases(mcNS).Get(context.Background(), leaseName(key), metav1.GetOptions{}); err != nil || *l.Spec.HolderIdentity != other {
		t.Fatalf("the successor's Lease is gone or changed: %v", err)
	}
}

// The holder's pod goes: a waiting pod takes over. Its volume, part
// streamed, is emptied before its plan says download.
func TestSingleFlightAWaitingPodTakesOverWhenTheHolderGoes(t *testing.T) {
	kube := fake.NewSimpleClientset()
	urls := map[string]string{}
	a, b := newFlightNode(t, kube, "a", urls), newFlightNode(t, kube, "b", urls)
	pa, pb := flightPod(t, "pa", "a", "ua"), flightPod(t, "pb", "b", "ub")
	addPod(t, kube, pa)
	addPod(t, kube, pb)
	emptyVolumes(t, map[*mcNode]*corev1.Pod{a: pa, b: pb})
	tickAll(a, b)
	if a.plan(pa) != modelcache.PlanDownload || b.plan(pb) != modelcache.PlanWait {
		t.Fatalf("plans %q %q", a.plan(pa), b.plan(pb))
	}
	a.downloadedLive(t, pa, map[string]string{"w.bin": "part"})
	mcEventually(t, "b streams", func() bool {
		tickAll(a, b)
		_, err := os.Stat(filepath.Join(b.volume(pb, "ngc-models"), "w.bin"))
		return err == nil
	})
	if err := kube.CoreV1().Pods(pa.Namespace).Delete(context.Background(), pa.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	mcEventually(t, "b takes over", func() bool { tickAll(a, b); return b.plan(pb) == modelcache.PlanDownload })
	ents, _ := os.ReadDir(b.volume(pb, "ngc-models"))
	if len(ents) != 0 {
		t.Fatalf("b's volume holds %d entries when its plan says download", len(ents))
	}
}

// The holder's agent restarts mid-download: the new one finds the Lease
// naming a pod on its node and keeps it, rather than let it expire into
// a second download.
func TestSingleFlightARestartedAgentKeepsItsPodsLease(t *testing.T) {
	kube := fake.NewSimpleClientset()
	urls := map[string]string{}
	a := newFlightNode(t, kube, "a", urls)
	pa := flightPod(t, "pa", "a", "ua")
	addPod(t, kube, pa)
	emptyVolumes(t, map[*mcNode]*corev1.Pod{a: pa})
	a.c.tick(context.Background())
	if a.plan(pa) != modelcache.PlanDownload {
		t.Fatalf("plan %q", a.plan(pa))
	}
	a.downloadedLive(t, pa, map[string]string{"w.bin": "half"})

	// A new controller for node a: same disk, no memory.
	a2 := newFlightNode(t, kube, "a", urls)
	a2.c.store, a2.c.podsRoot = a.c.store, a.c.podsRoot
	before, _ := kube.CoordinationV1().Leases(mcNS).Get(context.Background(), leaseName(keyOf(pa)), metav1.GetOptions{})
	a2.c.tick(context.Background())
	mcEventually(t, "the restarted agent renews", func() bool {
		l, err := kube.CoordinationV1().Leases(mcNS).Get(context.Background(), leaseName(keyOf(pa)), metav1.GetOptions{})
		return err == nil && *l.Spec.HolderIdentity == "a/ua" && !l.Spec.RenewTime.Equal(before.Spec.RenewTime)
	})
	a2.c.mu.Lock()
	f := a2.c.flights[pa.UID]
	a2.c.mu.Unlock()
	if f == nil || f.role != roleHolder {
		t.Fatalf("flight %+v; want the holder adopted", f)
	}
}
