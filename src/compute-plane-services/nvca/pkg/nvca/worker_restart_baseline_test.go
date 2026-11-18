/*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nvca

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakek8sclient "k8s.io/client-go/kubernetes/fake"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/dsx-ai-factory/nvcf/src/libraries/go/lib/pkg/icms-translate/translate/common"

	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvca/internal/util/k8sutil"
	nvcatypes "github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvca/pkg/types"
)

func TestRecordWorkerRestartBaselines(t *testing.T) {
	now := time.Now()
	newPod := func(name string, labels map[string]string, restarts int32, runFor time.Duration) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "nvcf-backend", Labels: labels},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: common.UtilsContainerName,
				Env:  []corev1.EnvVar{{Name: k8sutil.InstanceIDEnvKey, Value: "instance-1"}},
			}}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:         common.UtilsContainerName,
				RestartCount: restarts,
				Ready:        true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(now.Add(-runFor)),
				}},
			}}},
		}
	}
	reqLabels := map[string]string{nvcatypes.ICMSRequestIDKey: "req-1"}
	healthy := newPod("healthy", reqLabels, 4, time.Hour)
	recent := newPod("recent", reqLabels, 4, 5*time.Minute)
	unrelated := newPod("unrelated", nil, 4, time.Hour)

	client := fakek8sclient.NewSimpleClientset(healthy, recent, unrelated)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, p := range []*corev1.Pod{healthy, recent, unrelated} {
		require.NoError(t, indexer.Add(p))
	}

	recordWorkerRestartBaselines(context.Background(), client, listersv1.NewPodLister(indexer), now)

	got := map[string]string{}
	for _, name := range []string{"healthy", "recent", "unrelated"} {
		p, err := client.CoreV1().Pods("nvcf-backend").Get(context.Background(), name, metav1.GetOptions{})
		require.NoError(t, err)
		got[name] = p.Annotations[k8sutil.WorkerRestartCountBaselineAnnotationKey]
	}
	assert.Equal(t, map[string]string{"healthy": "4", "recent": "", "unrelated": ""}, got)
}
