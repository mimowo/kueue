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

package policy

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestTargetPreemptedUsage(t *testing.T) {
	cpu := resources.FlavorResource{Flavor: "default", Resource: corev1.ResourceCPU}
	podSet := func(name kueue.PodSetReference, count int32, milliCPU int64) workload.PodSetResources {
		return workload.PodSetResources{
			Name:     name,
			Count:    count,
			Requests: resources.NewRequestsFromMap(map[corev1.ResourceName]int64{corev1.ResourceCPU: milliCPU}),
			Flavors:  map[corev1.ResourceName]kueue.ResourceFlavorReference{corev1.ResourceCPU: cpu.Flavor},
		}
	}
	// The driver has a topology assignment, the executors do not.
	driver := podSet("driver", 1, 2_000)
	driver.TopologyRequest = &workload.TopologyRequest{
		Levels: []string{corev1.LabelHostname},
		DomainRequests: []workload.TopologyDomainRequests{{
			Values:            []string{"node-a"},
			SinglePodRequests: resources.NewRequestsFromMap(map[corev1.ResourceName]int64{corev1.ResourceCPU: 2_000}),
			Count:             1,
		}},
	}
	full := &workload.Info{TotalRequests: []workload.PodSetResources{driver, podSet("executor", 5, 5_000)}}
	reduced := &workload.Info{TotalRequests: []workload.PodSetResources{driver, podSet("executor", 2, 2_000)}}

	cases := map[string]struct {
		target *Target
		want   workload.Usage
	}{
		"an evicted target gives up all of its usage": {
			target: &Target{WorkloadInfo: full},
			want: workload.Usage{
				Quota: workload.ResourceUsage{
					Assigned:   resources.FlavorResourceQuantities{cpu: resources.NewAmount(7_000)},
					Unassigned: resources.MapRequests{},
				},
				TAS: workload.TASUsage{cpu.Flavor: driver.TopologyRequest.DomainRequests},
			},
		},
		"a partially preempted target gives up what it does not retain": {
			target: &Target{
				WorkloadInfo: full,
				PartialPreemption: &PartialPreemptionTarget{
					PodSet:        "executor",
					RetainedCount: 2,
					ReducedInfo:   reduced,
				},
			},
			want: workload.Usage{
				Quota: workload.ResourceUsage{
					Assigned:   resources.FlavorResourceQuantities{cpu: resources.NewAmount(3_000)},
					Unassigned: resources.MapRequests{},
				},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.target.PreemptedUsage()
			if diff := cmp.Diff(tc.want, got, cmp.Comparer(resources.Equal)); diff != "" {
				t.Errorf("PreemptedUsage() returned (-want,+got):\n%s", diff)
			}
		})
	}
}
