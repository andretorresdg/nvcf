// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"

	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/election"
	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/modelcache"
	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/modelid"
)

// Node-local model cache (docs/design/node-model-cache.md). A cold pod
// whose init downloads its model into an emptyDir gets that init wrapped
// by nvsnap-helper model-cache: with the node's cache of the same
// download mounted read-only into the init (and only the init), the
// helper seeds the emptyDir from a verified copy, or runs the download
// as the pod would have. The emptyDir and the main container are left as
// they are, so a capture still finds the download volume unused and a
// group restore still skips the download, and nothing that serves or is
// checkpointed ever mounts the cache.
const (
	// ModelCacheVolume is the pod volume of the node's cache directory for
	// the pod's download key.
	ModelCacheVolume = "nvsnap-model-cache"
	// ModelCacheMount is where the download init sees it.
	ModelCacheMount = "/nvsnap-model-cache"
	// ModelCacheKeyLabel carries the download's cache key, for the agent
	// on the pod's node (harvest, peer fetch, eviction pins).
	ModelCacheKeyLabel = "nvsnap.io/model-cache-key"
	// ModelCacheVolumeAnnotation names the download volume the agent
	// harvests from.
	ModelCacheVolumeAnnotation = "nvsnap.io/model-cache-volume"
	// ModelCacheInitAnnotation names the wrapped download init, whose
	// successful exit tells the agent the volume is complete.
	ModelCacheInitAnnotation = "nvsnap.io/model-cache-init"
)

// TenantScoper names the tenant of a namespace (tenant.Resolver).
type TenantScoper interface {
	Scope(ctx context.Context, ns string) string
}

// NodeModelCache configures the node-local model cache.
type NodeModelCache struct {
	// Root is the cache directory on every node.
	Root string
	// Wait bounds, in seconds, how long a download init waits for a peer
	// fetch the agent has in progress.
	Wait int
	// Tenants scopes keys: a tree downloaded with one tenant's
	// credentials is never seeded into another tenant's pod.
	Tenants TenantScoper
}

// recipeEnvPrefixes are the env vars that can change what a download
// writes. Others (instance names, namespaces) differ per deployment and
// would only split the key.
var recipeEnvPrefixes = []string{"NGC_", "HF_", "HUGGING", "MODEL", "NIM_"}

// nodeModelCachePatches wraps the pod's model download init, or returns
// nil when the pod has none the cache can serve.
func (m *Mutator) nodeModelCachePatches(ctx context.Context, pod *corev1.Pod) []PatchOp {
	c := m.NodeModelCache
	if c == nil || c.Root == "" || m.ModelVolume != nil || gpuRequest(pod) == 0 {
		return nil
	}
	d, ok := ResolveModelCacheDownload(ctx, c.Tenants, pod, m.MainContainer)
	if !ok || d.Wrapped {
		// A wrapped init is a pod admitted twice; leave it as it is.
		return nil
	}
	idx, init, land, key := d.InitIndex, &pod.Spec.InitContainers[d.InitIndex], d.Landing, d.Key
	log := m.logger().WithFields(logrus.Fields{"pod": election.PodIdentity(pod), "model": d.ShareKey, "key": key})

	to := initMountFor(init, land)
	wait := c.Wait
	if wait <= 0 {
		wait = 600
	}
	argv := append([]string{HelperBin, "model-cache", "--cache", ModelCacheMount, "--to", to, "--wait", strconv.Itoa(wait), "--"}, containerArgv(init)...)
	base := fmt.Sprintf("/spec/initContainers/%d", idx)
	patches := []PatchOp{{Op: "replace", Path: base + "/command", Value: argv}}
	if init.Args != nil {
		patches = append(patches, PatchOp{Op: "remove", Path: base + "/args"})
	}
	mounts := []corev1.VolumeMount{{Name: ModelCacheVolume, MountPath: ModelCacheMount, ReadOnly: true}, helperMount()}
	if init.VolumeMounts == nil {
		patches = append(patches, PatchOp{Op: "add", Path: base + "/volumeMounts", Value: mounts})
	} else {
		for _, vm := range mounts {
			patches = append(patches, PatchOp{Op: "add", Path: base + "/volumeMounts/-", Value: vm})
		}
	}
	dir := corev1.HostPathDirectoryOrCreate
	patches = append(patches, addVolumes(pod, []corev1.Volume{{Name: ModelCacheVolume, VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: path.Join(c.Root, key), Type: &dir}}}})...)
	patches = m.withHelperVolume(pod, patches)
	mp := newMetaPatcher(pod)
	patches = append(patches, mp.label(ModelCacheKeyLabel, key)...)
	patches = append(patches, mp.annotation(ModelCacheVolumeAnnotation, land.VolumeName)...)
	patches = append(patches, mp.annotation(ModelCacheInitAnnotation, init.Name)...)
	log.WithField("init", init.Name).Info("node model cache: download init seeds from the node's copy when it has one")
	return patches
}

// ModelCacheDownload is a pod's model download the node cache can serve.
type ModelCacheDownload struct {
	Key       string
	ShareKey  string
	Landing   modelid.Landing
	InitIndex int
	// Download is the init's own download command, with the helper's
	// wrapping removed.
	Download []string
	// Wrapped reports whether the init already runs under the helper.
	Wrapped bool
}

// modelCachePrefix starts a wrapped init's command.
var modelCachePrefix = []string{HelperBin, "model-cache"}

// ResolveModelCacheDownload finds the pod's model download init and its
// cache key, from the pod spec and the namespace's tenant only. The agent
// calls it too, on the admitted pod: it never takes a key from a label a
// pod could set itself, so no pod can put its files in another tenant's
// or another model's cache.
func ResolveModelCacheDownload(ctx context.Context, tenants TenantScoper, pod *corev1.Pod, mainContainer int) (ModelCacheDownload, bool) {
	var d ModelCacheDownload
	res, ok := modelid.Resolve(pod, mainContainer)
	if !ok || res.Identity.Scheme == "path" {
		return d, false
	}
	land := res.Landing
	if land.Downloader != modelid.DownloaderInit || land.Kind != modelid.VolumeEmptyDir || land.VolumeName == "" {
		return d, false
	}
	idx := -1
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == land.InitContainer {
			idx = i
		}
	}
	if idx < 0 {
		return d, false
	}
	init := pod.Spec.InitContainers[idx]
	argv := containerArgv(&init)
	if len(argv) >= 2 && argv[0] == modelCachePrefix[0] && argv[1] == modelCachePrefix[1] {
		dash := -1
		for i, a := range argv {
			if a == "--" {
				dash = i
				break
			}
		}
		if dash < 0 || dash == len(argv)-1 {
			return d, false
		}
		argv, d.Wrapped = argv[dash+1:], true
	} else if len(init.Command) == 0 || init.Command[0] == HelperBin {
		// Without its own command the init runs its image's entrypoint,
		// which cannot be wrapped; another helper step is not a download.
		return d, false
	}
	scope := "namespace:" + pod.Namespace
	if tenants != nil {
		scope = tenants.Scope(ctx, pod.Namespace)
	}
	init.Command, init.Args = argv, nil
	d.Key = modelcache.Key(scope, res.ShareKey(), init.Image, downloadRecipe(&init))
	d.ShareKey, d.Landing, d.InitIndex, d.Download = res.ShareKey(), land, idx, argv
	return d, true
}

// downloadRecipe is what, besides the image, decides the tree a download
// init writes: its command, arguments and the env vars that steer a
// download. Values from a Secret or field are named, not read.
func downloadRecipe(c *corev1.Container) string {
	var b strings.Builder
	for _, s := range containerArgv(c) {
		fmt.Fprintf(&b, "arg %d:%s\n", len(s), s)
	}
	var env []string
	for _, e := range c.Env {
		if !hasAnyPrefix(e.Name, recipeEnvPrefixes) {
			continue
		}
		v := e.Value
		if e.ValueFrom != nil {
			v = "<from " + valueFromRef(e.ValueFrom) + ">"
		}
		env = append(env, e.Name+"="+v)
	}
	sort.Strings(env)
	for _, s := range env {
		fmt.Fprintf(&b, "env %d:%s\n", len(s), s)
	}
	return b.String()
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func valueFromRef(v *corev1.EnvVarSource) string {
	switch {
	case v.SecretKeyRef != nil:
		return "secret " + v.SecretKeyRef.Name + "/" + v.SecretKeyRef.Key
	case v.ConfigMapKeyRef != nil:
		return "configmap " + v.ConfigMapKeyRef.Name + "/" + v.ConfigMapKeyRef.Key
	case v.FieldRef != nil:
		return "field " + v.FieldRef.FieldPath
	case v.ResourceFieldRef != nil:
		return "resource " + v.ResourceFieldRef.Resource
	}
	return "?"
}
