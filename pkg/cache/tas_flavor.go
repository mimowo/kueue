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
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/kueue/pkg/resources"
	utiltas "sigs.k8s.io/kueue/pkg/util/tas"
	"sigs.k8s.io/kueue/pkg/workload"
)

type TASCacheRF struct {
	sync.RWMutex
	informerCache cache.Cache
	NodeLabels    map[string]string
	Levels        []string
	usageMap      map[utiltas.TopologyDomainId]resources.Requests
}

func (tasRf *TASCacheRF) snapshot(ctx context.Context) *TASResourceFlavorSnapshot {
	nodeList := &corev1.NodeList{}
	err := tasRf.informerCache.List(ctx, nodeList)
	if err != nil {
		klog.ErrorS(err, "Failed to list nodes")
	}
	return tasRf.snapshotForNodes(nodeList.Items)
}

func (tasRf *TASCacheRF) snapshotForNodes(nodeList []corev1.Node) *TASResourceFlavorSnapshot {
	tasRf.RLock()
	defer tasRf.RUnlock()
	snapshot := newTASResourceFlavorSnapshot(tasRf.Levels)
	for _, node := range nodeList {
		if tasRf.matches(&node) {
			levelValues := levelValues(tasRf.Levels, node.Labels)
			capacity := resources.NewRequests(node.Status.Allocatable)
			domainId := utiltas.DomainId(levelValues)
			snapshot.memNodeLabels(domainId, levelValues)
			snapshot.addCapacity(domainId, capacity)
			klog.InfoS("TAS Node", "node", node.Name, "levelValues", levelValues, "domainId", domainId, "capacity", capacity)
		}
	}
	for domainId, usage := range tasRf.usageMap {
		snapshot.addUsage(domainId, usage)
	}
	snapshot.initialize()
	return snapshot
}

func (tasRf *TASCacheRF) addUsage(topologyRequests []workload.TopologyDomainRequests) {
	tasRf.updateUsage(topologyRequests, 1)
}

func (tasRf *TASCacheRF) removeUsage(topologyRequests []workload.TopologyDomainRequests) {
	tasRf.updateUsage(topologyRequests, -1)
}

func (tasRf *TASCacheRF) updateUsage(topologyRequests []workload.TopologyDomainRequests, m int64) {
	tasRf.Lock()
	defer tasRf.Unlock()
	for _, tr := range topologyRequests {
		levelValues := levelValues(tasRf.Levels, tr.NodeLabels)
		domainId := utiltas.DomainId(levelValues)
		_, found := tasRf.usageMap[domainId]
		if !found {
			tasRf.usageMap[domainId] = resources.Requests{}
		}
		if m < 0 {
			tasRf.usageMap[domainId].Sub(tr.Requests)
		} else {
			tasRf.usageMap[domainId].Add(tr.Requests)
		}
	}
}

func levelValues(levelKeys []string, labelsMap map[string]string) []string {
	levelValues := make([]string, len(levelKeys))
	for levelIdx, levelKey := range levelKeys {
		levelValues[levelIdx] = labelsMap[levelKey]
	}
	return levelValues
}

func (tasRf *TASCacheRF) matches(node *corev1.Node) bool {
	for labelKey, labelValue := range tasRf.NodeLabels {
		value, found := node.Labels[labelKey]
		if !found || value != labelValue {
			return false
		}
	}
	return true
}
