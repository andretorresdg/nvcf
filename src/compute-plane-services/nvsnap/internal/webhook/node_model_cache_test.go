// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"path"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/modelcache"
	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/modelvolume"
)

type fixedTenants map[string]string

func (f fixedTenants) Scope(_ context.Context, ns string) string {
	if s, ok := f[ns]; ok {
		return s
	}
	return "namespace:" + ns
}

func nodeCacheMutator(t *testing.T) *Mutator {
	t.Helper()
	return &Mutator{
		Backend:        newBackend(t),
		NodeModelCache: &NodeModelCache{Root: "/var/lib/nvsnap-model-cache", Wait: 600, Tenants: fixedTenants{"sr-fn": "tenant:nca=a"}},
	}
}

func initByName(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

func TestNodeModelCacheWrapsTheDownloadInit(t *testing.T) {
	m := nodeCacheMutator(t)
	pod := ngcFunctionPod()
	orig := containerArgv(initByName(pod, "download-ngc-model"))
	out := applyPatches(t, pod, m.nodeModelCachePatches(context.Background(), pod, true))

	key := out.Labels[ModelCacheKeyLabel]
	if !modelcache.ValidKey(key) {
		t.Fatalf("key label = %q", key)
	}
	if out.Annotations[ModelCacheVolumeAnnotation] != "ngc-models" || out.Annotations[ModelCacheInitAnnotation] != "download-ngc-model" {
		t.Fatalf("annotations = %v", out.Annotations)
	}
	init := initByName(out, "download-ngc-model")
	want := append([]string{HelperBin, "model-cache", "--cache", ModelCacheMount, "--to", "/config/models", "--wait", "600", "--"}, orig...)
	if !reflect.DeepEqual(init.Command, want) || init.Args != nil {
		t.Fatalf("init command = %q args %q\nwant %q", init.Command, init.Args, want)
	}
	var cacheRO, helper bool
	for _, vm := range init.VolumeMounts {
		cacheRO = cacheRO || (vm.Name == ModelCacheVolume && vm.MountPath == ModelCacheMount && vm.ReadOnly)
		helper = helper || vm.Name == helperVolumeName
	}
	if !cacheRO || !helper {
		t.Fatalf("init mounts = %+v; want the cache read-only and the helper", init.VolumeMounts)
	}
	var vol *corev1.Volume
	for i := range out.Spec.Volumes {
		if out.Spec.Volumes[i].Name == ModelCacheVolume {
			vol = &out.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.HostPath == nil || vol.HostPath.Path != path.Join("/var/lib/nvsnap-model-cache", key) {
		t.Fatalf("cache volume = %+v; want hostPath <root>/<key>", vol)
	}
	// The main container and the download volume are untouched: restore
	// and serving never see the cache.
	if !reflect.DeepEqual(out.Spec.Containers, pod.Spec.Containers) {
		t.Fatal("the main container changed")
	}
	for _, v := range out.Spec.Volumes {
		if v.Name == "ngc-models" && v.EmptyDir == nil {
			t.Fatal("the download volume is no longer an emptyDir")
		}
	}
}

func TestNodeModelCacheKeyIsPerTenantAndPerRecipe(t *testing.T) {
	m := nodeCacheMutator(t)
	key := func(mut func(*corev1.Pod)) string {
		pod := ngcFunctionPod()
		mut(pod)
		return applyPatches(t, pod, m.nodeModelCachePatches(context.Background(), pod, true)).Labels[ModelCacheKeyLabel]
	}
	base := key(func(*corev1.Pod) {})
	same := key(func(p *corev1.Pod) {
		// Per-deployment env the download does not use does not split the key.
		initByName(p, "download-ngc-model").Env = append(initByName(p, "download-ngc-model").Env, corev1.EnvVar{Name: "GROVE_HEADLESS_SERVICE", Value: "x.other-ns.svc"})
	})
	if same != base {
		t.Fatal("unrelated env changed the key")
	}
	for name, mut := range map[string]func(*corev1.Pod){
		"tenant": func(p *corev1.Pod) { p.Namespace = "other" },
		"image":  func(p *corev1.Pod) { initByName(p, "download-ngc-model").Image = "nvcr.io/org/ultra:vllm2" },
		"command": func(p *corev1.Pod) {
			initByName(p, "download-ngc-model").Args = []string{"ngc registry model download-version --dest /config/models x"}
		},
		"model": func(p *corev1.Pod) {
			for _, c := range []*corev1.Container{initByName(p, "download-ngc-model")} {
				c.Env[0].Value = "org/team/other:1"
			}
		},
		"any other env": func(p *corev1.Pod) {
			initByName(p, "download-ngc-model").Env = append(initByName(p, "download-ngc-model").Env, corev1.EnvVar{Name: "STORE_ENDPOINT", Value: "https://other"})
		},
		"envFrom": func(p *corev1.Pod) {
			initByName(p, "download-ngc-model").EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "dl"}}}}
		},
		"a mount": func(p *corev1.Pod) {
			for i := range p.Spec.Volumes {
				if p.Spec.Volumes[i].Name == "scripts" {
					p.Spec.Volumes[i].ConfigMap.Name = "other-scripts"
				}
			}
		},
		"envFrom order": func(p *corev1.Pod) {
			c := initByName(p, "download-ngc-model")
			c.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "b"}}},
				{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "a"}}}}
		},
		"fsGroup": func(p *corev1.Pod) {
			g := int64(2000)
			p.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: &g}
		},
		"configmap items": func(p *corev1.Pod) {
			for i := range p.Spec.Volumes {
				if p.Spec.Volumes[i].Name == "scripts" {
					p.Spec.Volumes[i].ConfigMap.Items = []corev1.KeyToPath{{Key: "v2.sh", Path: "start.sh"}}
				}
			}
		},
		"ngc env": func(p *corev1.Pod) {
			initByName(p, "download-ngc-model").Env = append(initByName(p, "download-ngc-model").Env, corev1.EnvVar{Name: "NGC_CLI_MAX_CONCURRENCY", Value: "3"})
		},
	} {
		if k := key(mut); k == base || k == "" {
			t.Fatalf("%s: key %q did not change", name, k)
		}
	}
}

func TestNodeModelCacheLeavesOtherPodsAlone(t *testing.T) {
	m := nodeCacheMutator(t)
	for name, pod := range map[string]*corev1.Pod{
		"engine downloads itself": stockVLLMPod(),
		"no gpu": func() *corev1.Pod {
			p := ngcFunctionPod()
			p.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
			return p
		}(),
		"init without a command": func() *corev1.Pod {
			p := ngcFunctionPod()
			initByName(p, "download-ngc-model").Command = nil
			return p
		}(),
		"landing is the customer's claim": func() *corev1.Pod {
			p := ngcFunctionPod()
			for i := range p.Spec.Volumes {
				if p.Spec.Volumes[i].Name == "ngc-models" {
					p.Spec.Volumes[i].VolumeSource = corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared"}}
				}
			}
			return p
		}(),
		"subPath download mount": func() *corev1.Pod {
			p := ngcFunctionPod()
			c := initByName(p, "download-ngc-model")
			c.VolumeMounts[0].SubPath = "data"
			return p
		}(),
		"already wrapped": func() *corev1.Pod {
			p := ngcFunctionPod()
			c := initByName(p, "download-ngc-model")
			c.Command = append([]string{HelperBin, "model-cache"}, c.Command...)
			return p
		}(),
	} {
		if ops := m.nodeModelCachePatches(context.Background(), pod, true); ops != nil {
			t.Errorf("%s: patched %v", name, ops)
		}
	}

	off := nodeCacheMutator(t)
	off.NodeModelCache = nil
	if ops := off.nodeModelCachePatches(context.Background(), ngcFunctionPod(), true); ops != nil {
		t.Error("feature off: patched")
	}
	if ops := nodeCacheMutator(t).nodeModelCachePatches(context.Background(), ngcFunctionPod(), false); ops != nil {
		t.Error("a pod not on the node bundle: patched")
	}
	both := nodeCacheMutator(t)
	both.ModelVolume = &modelvolume.Provisioner{}
	if ops := both.nodeModelCachePatches(context.Background(), ngcFunctionPod(), true); ops != nil {
		t.Error("model volumes on: the cache patched too")
	}
}

// Wiring: Mutate, the production entry point, applies the cache to a cold
// NGC pod.
func TestMutateAppliesTheNodeModelCache(t *testing.T) {
	m := nodeCacheMutator(t)
	m.HostBundleRoot = "/bundle"
	// Without the gpushare library the pod does not use the node bundle,
	// and is left alone.
	plain := ngcFunctionPod()
	ops, err := m.Mutate(context.Background(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if c := initByName(applyPatches(t, plain, ops), "download-ngc-model"); len(c.Command) > 0 && c.Command[0] == HelperBin {
		t.Fatal("Mutate wrapped the init of a pod that does not use the node bundle")
	}
	pod := ngcFunctionPod()
	pod.Annotations = map[string]string{GPUShareAnnotation: "true"}
	ops, err = m.Mutate(context.Background(), pod)
	if err != nil {
		t.Fatal(err)
	}
	out := applyPatches(t, pod, ops)
	init := initByName(out, "download-ngc-model")
	if len(init.Command) < 2 || init.Command[0] != HelperBin || init.Command[1] != "model-cache" {
		t.Fatalf("Mutate did not wrap the download init: %q", init.Command)
	}
	if !strings.Contains(strings.Join(init.Command, " "), "download-version") {
		t.Fatalf("the original download is not the fallback: %q", init.Command)
	}
}

// The agent recomputes the key from the admitted (wrapped) pod; it must
// match the key admission gave the unwrapped pod, and must not follow a
// label the pod sets itself.
func TestModelCacheKeyIsTheSameBeforeAndAfterWrapping(t *testing.T) {
	m := nodeCacheMutator(t)
	pod := ngcFunctionPod()
	before, ok := ResolveModelCacheDownload(context.Background(), m.NodeModelCache.Tenants, pod, 0)
	if !ok || before.Wrapped {
		t.Fatalf("unwrapped: %+v ok=%v", before, ok)
	}
	out := applyPatches(t, pod, m.nodeModelCachePatches(context.Background(), pod, true))
	out.Labels[ModelCacheKeyLabel] = "ffffffffffffffffffffffffffffffff" // a pod's own claim
	after, ok := ResolveModelCacheDownload(context.Background(), m.NodeModelCache.Tenants, out, 0)
	if !ok || !after.Wrapped || after.Key != before.Key {
		t.Fatalf("wrapped: key %q wrapped=%v ok=%v; want key %q", after.Key, after.Wrapped, ok, before.Key)
	}
	if !reflect.DeepEqual(after.Download, containerArgv(initByName(pod, "download-ngc-model"))) {
		t.Fatalf("download = %q", after.Download)
	}
}

// Another writable volume the init mounts and a later container reads may
// be an output: the helper is told to watch it.
func TestNodeModelCacheWatchesOtherWritableVolumes(t *testing.T) {
	m := nodeCacheMutator(t)
	p := ngcFunctionPod()
	p.Spec.Volumes = append(p.Spec.Volumes,
		corev1.Volume{Name: "rendered", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: "ro-pvc", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "in", ReadOnly: true}}})
	c := initByName(p, "download-ngc-model")
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "rendered", MountPath: "/config/rendered"}, corev1.VolumeMount{Name: "ro-pvc", MountPath: "/in"})
	p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "rendered", MountPath: "/r"}, corev1.VolumeMount{Name: "ro-pvc", MountPath: "/in"})
	out := applyPatches(t, p, m.nodeModelCachePatches(context.Background(), p, true))
	cmd := strings.Join(initByName(out, "download-ngc-model").Command, " ")
	if !strings.Contains(cmd, "--watch /config/rendered --") || strings.Contains(cmd, "--watch /in") {
		t.Fatalf("command %q: want /config/rendered watched and the read-only claim not", cmd)
	}
}

// A writable volume an earlier init filled is an input of the download,
// not an output: the pod stays eligible (NVCA's merged certificates).
func TestNodeModelCacheAcceptsAnInputVolumeFromAnEarlierInit(t *testing.T) {
	m := nodeCacheMutator(t)
	p := ngcFunctionPod()
	p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "certs", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	certs := corev1.VolumeMount{Name: "certs", MountPath: "/etc/ssl/certs"}
	p.Spec.InitContainers = append([]corev1.Container{{Name: "merge-certs", Image: "x", Command: []string{"merge"}, VolumeMounts: []corev1.VolumeMount{certs}}}, p.Spec.InitContainers...)
	c := initByName(p, "download-ngc-model")
	c.VolumeMounts = append(c.VolumeMounts, certs)
	p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, certs)
	if ops := m.nodeModelCachePatches(context.Background(), p, true); ops == nil {
		t.Fatal("a pod whose download reads an earlier init's volume was left alone")
	}
}

func TestModelCacheKeyFollowsEnvFromOrder(t *testing.T) {
	key := func(names ...string) string {
		p := ngcFunctionPod()
		c := initByName(p, "download-ngc-model")
		for _, n := range names {
			c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: n}}})
		}
		d, ok := ResolveModelCacheDownload(context.Background(), nil, p, 0)
		if !ok {
			t.Fatal("not a cache download")
		}
		return d.Key
	}
	if key("a", "b") == key("b", "a") {
		t.Fatal("envFrom order does not change the key; the last source wins in Kubernetes")
	}
}

// A pod whose gpushare placement is already in its spec uses the bundle
// even though the gpushare step adds nothing.
func TestMutateTreatsAnAlreadyPlacedBundleAsPlaced(t *testing.T) {
	m := nodeCacheMutator(t)
	m.HostBundleRoot = "/bundle"
	pod := ngcFunctionPod()
	pod.Spec.Volumes = append(pod.Spec.Volumes, m.helperVolume())
	ops, err := m.Mutate(context.Background(), pod)
	if err != nil {
		t.Fatal(err)
	}
	if c := initByName(applyPatches(t, pod, ops), "download-ngc-model"); len(c.Command) < 2 || c.Command[1] != "model-cache" {
		t.Fatalf("a pod already on the bundle was not wrapped: %q", c.Command)
	}
}

// Pods of one instance differ in the names of their service account token
// volumes; that must not give each its own key.
func TestModelCacheKeyIgnoresPodLocalVolumeNames(t *testing.T) {
	key := func(tokenVol string) string {
		p := ngcFunctionPod()
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: tokenVol, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}}}}}}})
		c := initByName(p, "download-ngc-model")
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: tokenVol, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
		d, ok := ResolveModelCacheDownload(context.Background(), nil, p, 0)
		if !ok {
			t.Fatal("not a cache download")
		}
		return d.Key
	}
	if key("kube-api-access-mh6zm") != key("kube-api-access-sbg6z") {
		t.Fatal("a pod-local volume name changed the key")
	}
}
