// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/webhook"
)

// A redeploy that changes a member's process configuration (a new
// DYN_NAMESPACE_WORKER_SUFFIX) leaves a record its pods may not restore
// from: the next Ready instance captures again rather than start cold for
// good.

// stamped is p with the process configuration admission stamps, the env of
// its container ctr ("" for its GPU container) set to env.
func stamped(p corev1.Pod, ctr string, env ...corev1.EnvVar) *corev1.Pod {
	q := p.DeepCopy()
	if ctr == "" {
		ctr = gpuContainer(q)
	}
	for i := range q.Spec.Containers {
		if q.Spec.Containers[i].Name == ctr {
			q.Spec.Containers[i].Env = env
		}
	}
	if q.Annotations == nil {
		q.Annotations = map[string]string{}
	}
	q.Annotations[webhook.ProcessConfigAnnotation] = webhook.ProcessConfig(q)
	return q
}

func reconfigRecord(members []*corev1.Pod, ranks int, companionContainers map[string]string) map[string]string {
	data := map[string]string{}
	for i, m := range members {
		ctr := gpuContainer(m)
		if i >= ranks {
			ctr = companionContainers[m.Name]
		}
		recordProcessConfig(data, i, m, ctr, len(members))
	}
	return data
}

func TestProcessConfigChangedNamesTheChangedMember(t *testing.T) {
	r0 := stsRank("kimi-0", "0", "2", "sts-c", true)
	r1 := stsRank("kimi-1", "1", "2", "sts-c", true)
	suffix := func(v string) corev1.EnvVar { return corev1.EnvVar{Name: "DYN_NAMESPACE_WORKER_SUFFIX", Value: v} }
	old := []*corev1.Pod{stamped(r0, "", suffix("a1")), stamped(r1, "", suffix("a1"))}
	data := reconfigRecord(old, 2, nil)

	if got := processConfigChanged(data, old, 2, nil); len(got) != 0 {
		t.Fatalf("the instance the record was taken from: changed %q", got)
	}
	redeployed := []*corev1.Pod{stamped(r0, "", suffix("b7")), stamped(r1, "", suffix("b7"))}
	got := processConfigChanged(data, redeployed, 2, nil)
	if len(got) == 0 || !strings.Contains(strings.Join(got, " "), "DYN_NAMESPACE_WORKER_SUFFIX") {
		t.Fatalf("a redeploy with a new suffix: changed %q", got)
	}
	// A slot the record holds no configuration for (a capture from before
	// they were recorded) is not called changed: nothing says it is.
	delete(data, "processConfig.0")
	delete(data, "processConfig.1")
	if got := processConfigChanged(data, redeployed, 2, nil); len(got) != 0 {
		t.Fatalf("no recorded configuration: changed %q", got)
	}
}

func TestProcessConfigChangedComparesACompanionByItsContainer(t *testing.T) {
	r0 := stsRank("kimi-0", "0", "1", "sts-c", true)
	etcd := stsRank("etcd-0", "0", "1", "sts-e", false)
	etcd.Name = "etcd-0"
	cc := map[string]string{"etcd-0": etcd.Spec.Containers[0].Name}
	old := []*corev1.Pod{stamped(r0, ""), stamped(etcd, cc["etcd-0"], corev1.EnvVar{Name: "ETCD_X", Value: "1"})}
	data := reconfigRecord(old, 1, cc)
	now := []*corev1.Pod{stamped(r0, ""), stamped(etcd, cc["etcd-0"], corev1.EnvVar{Name: "ETCD_X", Value: "2"})}
	if got := processConfigChanged(data, now, 1, cc); len(got) == 0 {
		t.Fatal("a companion's changed configuration is not seen")
	}
}

func TestClaimGroupRecord_ReplacesARecordOfAnotherConfiguration(t *testing.T) {
	c, kc, _ := groupFixture(t)
	ctx := context.Background()
	p := stsRank("kimi-0", "0", "2", "sts-c", true)
	changed := func(map[string]string) bool { return true }
	same := func(map[string]string) bool { return false }

	setRecord(t, kc, map[string]string{"state": "complete", "captures": "3"})
	if again, err := c.claimGroupRecord(ctx, "k1", 0, &p, 2, same); err != nil || again != nil {
		t.Fatalf("claim of a complete record of the same configuration = %v, %v", again, err)
	}
	claimed, err := c.claimGroupRecord(ctx, "k1", 0, &p, 2, changed)
	if err != nil || claimed == nil {
		t.Fatalf("claim of a complete record of another configuration = %v, %v; want it replaced", claimed, err)
	}
	// A new configuration gets its own attempts.
	if claimed.Data["state"] != criuGroupStateCapturing || claimed.Data["captures"] != "1" {
		t.Errorf("record = %v; want a first capture under way", claimed.Data)
	}
	for _, st := range []string{criuGroupStateCapturing, criuGroupStateVerifying} {
		setRecord(t, kc, map[string]string{"state": st, "captures": "1", "startedAt": "2999-01-01T00:00:00Z", "sourceNamespace": "fn"})
		if again, err := c.claimGroupRecord(ctx, "k1", 0, &p, 2, changed); err != nil || again != nil {
			t.Errorf("claim of a %s record = %v, %v; two configurations racing for one key: the first keeps it", st, again, err)
		}
	}
}
