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

package workload

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/utils/ptr"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/resources"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
)

// IsPartialPreemptionJob reports the cheap eligibility gate for partial preemption:
//   - the PartialPreemption feature gate is enabled, and
//   - the job explicitly opted in via the partial-preemption annotation (propagated to the
//     Workload).
func IsPartialPreemptionJob(wl *kueue.Workload) bool {
	return features.Enabled(features.PartialPreemption) &&
		wl.GetAnnotations()[constants.PartialPreemptionAnnotation] == "true"
}

// PartialPreemptionPodSet returns the single PodSet that can be shrunk.
func PartialPreemptionPodSet(wl *kueue.Workload, requests []PodSetResources) (kueue.PodSetReference, bool) {
	if !IsPartialPreemptionJob(wl) || workloadevict.IsEvicted(wl) {
		return "", false
	}

	var shrinkablePodSet kueue.PodSetReference
	for i := range wl.Spec.PodSets {
		ps := &wl.Spec.PodSets[i]
		if ps.PartialPreemption == nil || ps.PartialPreemption.MinRetainedCount == nil {
			continue
		}
		for j := range requests {
			psr := &requests[j]
			if psr.Name != ps.Name || psr.Count <= *ps.PartialPreemption.MinRetainedCount {
				continue
			}
			if psr.TopologyRequest != nil || shrinkablePodSet != "" {
				return "", false
			}
			shrinkablePodSet = ps.Name
		}
	}
	return shrinkablePodSet, shrinkablePodSet != ""
}

// HasPendingPartialPreemption reports whether a partial preemption of the
// workload is pending: a PodSet still has more Pods than its retainedCount. The
// PodSet has the smaller of its count in the spec, after reclaim, and its
// admitted count.
func HasPendingPartialPreemption(wl *kueue.Workload) bool {
	if !IsPartialPreemptionJob(wl) || wl.Status.Admission == nil {
		return false
	}
	effectiveCounts := podSetsCountsAfterReclaim(wl)
	for i := range wl.Status.Admission.PodSetAssignments {
		psa := &wl.Status.Admission.PodSetAssignments[i]
		if psa.PartialPreemption == nil || psa.PartialPreemption.RetainedCount == nil {
			continue
		}
		effective := min(effectiveCounts[psa.Name], ptr.Deref(psa.Count, effectiveCounts[psa.Name]))
		if effective > *psa.PartialPreemption.RetainedCount {
			return true
		}
	}
	return false
}

// PreemptionRetainedCount returns the requested partial-preemption target count for the named PodSet.
func PreemptionRetainedCount(wl *kueue.Workload, podSetName kueue.PodSetReference) (int32, bool) {
	if wl.Status.Admission == nil {
		return 0, false
	}
	for i := range wl.Status.Admission.PodSetAssignments {
		psa := &wl.Status.Admission.PodSetAssignments[i]
		if psa.Name == podSetName && psa.PartialPreemption != nil && psa.PartialPreemption.RetainedCount != nil {
			return *psa.PartialPreemption.RetainedCount, true
		}
	}
	return 0, false
}

// PreemptionMinRetainedCount returns the configured partial-preemption floor for the named PodSet.
func PreemptionMinRetainedCount(wl *kueue.Workload, podSetName kueue.PodSetReference) (int32, bool) {
	for i := range wl.Spec.PodSets {
		ps := &wl.Spec.PodSets[i]
		if ps.Name == podSetName && ps.PartialPreemption != nil && ps.PartialPreemption.MinRetainedCount != nil {
			return *ps.PartialPreemption.MinRetainedCount, true
		}
	}
	return 0, false
}

// ScaledResourceUsage returns the resourceUsage of a PodSet assignment whose
// count goes from count to newCount: the usage of one Pod times newCount. The
// elastic Job ungater releases quota with it, and the Workload webhook
// validates the release with it.
func ScaledResourceUsage(usage corev1.ResourceList, count, newCount int32) resources.Requests {
	return resources.NewRequestsFromResourceList(usage).ScaledDown(int64(count)).ScaledUp(int64(newCount))
}

// CanCompletePartialPreemption tells whether a partial preemption of the
// workload is in progress and every PodSet is down to its retained count.
func CanCompletePartialPreemption(wl *kueue.Workload) bool {
	return IsPartialPreemptionJob(wl) && wl.Status.Admission != nil &&
		meta.IsStatusConditionTrue(wl.Status.Conditions, kueue.WorkloadPartiallyPreempted) &&
		!HasPendingPartialPreemption(wl)
}

// ReleaseSatisfiedPreemptionTargets updates the partial-preemption condition after
// every partial-preemption request has been satisfied. The retainedCount field is
// intentionally left in place: it records Kueue's decision and has no further
// effect once count <= retainedCount.
func ReleaseSatisfiedPreemptionTargets(wl *kueue.Workload, now time.Time) bool {
	if !CanCompletePartialPreemption(wl) {
		return false
	}
	SetPartiallyPreemptedCondition(wl, now, false,
		kueue.WorkloadPartiallyPreemptedReasonQuotaReleased,
		"Partial-preemption request satisfied; quota released")
	return true
}
