/*
Copyright 2024 The Kubernetes Authors.

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

package cache

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/resources"
)

func TestTASSnapshot(t *testing.T) {
	const (
		tasBlockLabel = "cloud.com/topology-block"
		tasRackLabel  = "cloud.com/topology-rack"
	)

	defaultTestNodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "b1-r1-x1",
				Labels: map[string]string{
					tasBlockLabel: "b1",
					tasRackLabel:  "r1",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "b1-r2-x1",
				Labels: map[string]string{
					tasBlockLabel: "b1",
					tasRackLabel:  "r2",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "b1-r2-x2",
				Labels: map[string]string{
					tasBlockLabel: "b1",
					tasRackLabel:  "r2",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "b1-r2-x3",
				Labels: map[string]string{
					tasBlockLabel: "b1",
					tasRackLabel:  "r2",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "b2-r1-x1",
				Labels: map[string]string{
					tasBlockLabel: "b2",
					tasRackLabel:  "r1",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "b2-r2-x1",
				Labels: map[string]string{
					tasBlockLabel: "b2",
					tasRackLabel:  "r2",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		},
	}
	defaultTestLevels := []string{
		tasBlockLabel,
		tasRackLabel,
	}

	cases := map[string]struct {
		request        kueue.PodSetTopologyRequest
		levels         []string
		nodeLabels     map[string]string
		nodes          []corev1.Node
		requests       resources.Requests
		count          int32
		wantAssignment *kueue.TopologyAssignment
	}{
		"rack required; single Pod fits in a rack": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Required: ptr.To(tasRackLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceCPU: 1000, // "1"
			},
			count: 1,
			wantAssignment: &kueue.TopologyAssignment{
				Domains: []kueue.TopologyDomainAssignment{
					{
						Count: 1,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b1",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "b2",
							},
						},
					},
				},
			},
		},
	}
	for name, _ := range cases {
		t.Run(name, func(t *testing.T) {
			// tasFlavors := NewTASResourceFlavors(informerCache)
			// tasFlavor := tasFlavors.NewTASResourceFlavor(tc.levels, tc.nodeLabels)
			// tasSnapshot := tasFlavor.snapshot(ctx)
			// gotAssignment := tasSnapshot.FindTopologyAssignment(&tc.request, tc.requests, tc.count)
			// if diff := cmp.Diff(tc.wantAssignment, gotAssignment); diff != "" {
			// 	t.Errorf("unexpected topology assignment: %s", diff)
			// }
		})
	}
}
