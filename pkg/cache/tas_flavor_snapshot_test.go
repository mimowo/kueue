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

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/resources"
	utiltas "sigs.k8s.io/kueue/pkg/util/tas"
)

func TestFindTopologyAssignment(t *testing.T) {
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
								NodeLabelValue: "r2",
							},
						},
					},
				},
			},
		},
		"rack required; multiple Pods fits in a rack": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Required: ptr.To(tasRackLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceCPU: 1000, // "1"
			},
			count: 3,
			wantAssignment: &kueue.TopologyAssignment{
				Domains: []kueue.TopologyDomainAssignment{
					{
						Count: 3,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b1",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r2",
							},
						},
					},
				},
			},
		},
		"rack required; too many pods to fit in any rack": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Required: ptr.To(tasRackLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceCPU: 1000, // "1"
			},
			count:          4,
			wantAssignment: nil,
		},
		"block required; single Pod fits in a block": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Required: ptr.To(tasBlockLabel),
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
								NodeLabelValue: "r2",
							},
						},
					},
				},
			},
		},
		"block required; Pods fit in a block spread across two racks": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Required: ptr.To(tasBlockLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceCPU: 1000, // "1"
			},
			count: 4,
			wantAssignment: &kueue.TopologyAssignment{
				Domains: []kueue.TopologyDomainAssignment{
					{
						Count: 3,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b1",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r2",
							},
						},
					},
					{
						Count: 1,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b1",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r1",
							},
						},
					},
				},
			},
		},
		"block required; single Pod which cannot be split": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Required: ptr.To(tasBlockLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceCPU: 4000,
			},
			count:          1,
			wantAssignment: nil,
		},
		"block required; too many Pods to fit requested": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Required: ptr.To(tasBlockLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceCPU: 1000,
			},
			count:          5,
			wantAssignment: nil,
		},
		"rack required; single Pod requiring memory": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Required: ptr.To(tasRackLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceMemory: 1024, // "1"
			},
			count: 4,
			wantAssignment: &kueue.TopologyAssignment{
				Domains: []kueue.TopologyDomainAssignment{
					{
						Count: 4,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b2",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r2",
							},
						},
					},
				},
			},
		},
		"rack preferred; but only block can accommodate the workload": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Preferred: ptr.To(tasRackLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceCPU: 1000,
			},
			count: 4,
			wantAssignment: &kueue.TopologyAssignment{
				Domains: []kueue.TopologyDomainAssignment{
					{
						Count: 3,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b1",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r2",
							},
						},
					},
					{
						Count: 1,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b1",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r1",
							},
						},
					},
				},
			},
		},
		"rack preferred; but only multiple blocks can accommodate the workload": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Preferred: ptr.To(tasRackLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceCPU: 1000,
			},
			count: 6,
			wantAssignment: &kueue.TopologyAssignment{
				Domains: []kueue.TopologyDomainAssignment{
					{
						Count: 3,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b1",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r2",
							},
						},
					},
					{
						Count: 2,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b2",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r2",
							},
						},
					},
					{
						Count: 1,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b1",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r1",
							},
						},
					},
				},
			},
		},
		"block preferred; but only multiple blocks can accommodate the workload": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Preferred: ptr.To(tasBlockLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceCPU: 1000,
			},
			count: 6,
			wantAssignment: &kueue.TopologyAssignment{
				Domains: []kueue.TopologyDomainAssignment{
					{
						Count: 3,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b1",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r2",
							},
						},
					},
					{
						Count: 2,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b2",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r2",
							},
						},
					},
					{
						Count: 1,
						Levels: []kueue.TopologyDomainAssignmentLevel{
							{
								NodeLabel:      tasBlockLabel,
								NodeLabelValue: "b1",
							},
							{
								NodeLabel:      tasRackLabel,
								NodeLabelValue: "r1",
							},
						},
					},
				},
			},
		},
		"block preferred; but the workload cannot be accommodate in entire topology": {
			nodes: defaultTestNodes,
			request: kueue.PodSetTopologyRequest{
				Preferred: ptr.To(tasBlockLabel),
			},
			levels: defaultTestLevels,
			requests: resources.Requests{
				corev1.ResourceCPU: 1000,
			},
			count:          10,
			wantAssignment: nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot := newTASResourceFlavorSnapshot(tc.levels)

			for _, node := range tc.nodes {
				levelValues := levelValues(tc.levels, node.Labels)
				capacity := resources.NewRequests(node.Status.Allocatable)
				domainId := utiltas.DomainId(levelValues)
				snapshot.memNodeLabels(domainId, levelValues)
				snapshot.addCapacity(domainId, capacity)
				//			klog.InfoS("TAS Node", "node", node.Name, "levelValues", levelValues, "domainId", domainId, "capacity", capacity)
			}

			snapshot.initialize()
			gotAssignment := snapshot.FindTopologyAssignment(&tc.request, tc.requests, tc.count)
			if diff := cmp.Diff(tc.wantAssignment, gotAssignment); diff != "" {
				t.Errorf("unexpected topology assignment: %s", diff)
			}
		})
	}
}
