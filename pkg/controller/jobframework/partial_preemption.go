/*
Copyright The Kubernetes Authors.

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

package jobframework

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/util/parallelize"
	"sigs.k8s.io/kueue/pkg/workload"
)

// reconcilePartialPreemption evicts the Pods that a pending partial preemption
// of the admitted wl takes from the job, so that each PodSet drains to its
// retained count. The integration chooses the Pods (see JobWithPartialPreemption);
// without it nothing is evicted.
// Eviction goes through the Eviction API so PodDisruptionBudgets are honored.
func (r *JobReconciler) reconcilePartialPreemption(ctx context.Context, job GenericJob, wl *kueue.Workload) error {
	ppJob, implemented := job.(JobWithPartialPreemption)
	if !implemented || !workload.HasPendingPartialPreemption(wl) {
		return nil
	}
	targets := make(map[kueue.PodSetReference]int32)
	for i := range wl.Status.Admission.PodSetAssignments {
		psa := &wl.Status.Admission.PodSetAssignments[i]
		if psa.PartialPreemption != nil && psa.PartialPreemption.RetainedCount != nil {
			targets[psa.Name] = *psa.PartialPreemption.RetainedCount
		}
	}
	pods, err := ppJob.PodsToReclaim(ctx, r.client, wl, targets)
	if err != nil {
		return err
	}
	log := ctrl.LoggerFrom(ctx)
	return parallelize.Until(ctx, len(pods), func(i int) error {
		return evictPodForReclaim(ctx, r.client, log, pods[i])
	})
}

// evictPodForReclaim evicts one Pod through the Eviction API.
func evictPodForReclaim(ctx context.Context, c client.Client, log logr.Logger, pod *corev1.Pod) error {
	eviction := &policyv1.Eviction{
		Name:      pod.Name,
		Namespace: pod.Namespace,
	}
	if err := c.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		if apierrors.IsTooManyRequests(err) {
			log.V(3).Info("partial-preemption eviction blocked by PodDisruptionBudget, will retry", "pod", klog.KObj(pod))
			return err
		}
		return fmt.Errorf("evicting pod %s: %w", klog.KObj(pod), err)
	}
	log.V(3).Info("evicted elastic pod above partial-preemption target", "pod", klog.KObj(pod))
	return nil
}
