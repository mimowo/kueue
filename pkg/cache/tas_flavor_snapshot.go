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
	"slices"
	"strings"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/resources"
	utiltas "sigs.k8s.io/kueue/pkg/util/tas"
)

type info struct {
	count          int32
	domainId       utiltas.TopologyDomainId
	parentDomainId utiltas.TopologyDomainId
	childInfoIds   []utiltas.TopologyDomainId
}

// map from hash to countNode
type infoPerDomain map[utiltas.TopologyDomainId]*info

type TASResourceFlavorSnapshot struct {
	levelKeys            []string
	capacityPerDomain    map[utiltas.TopologyDomainId]resources.Requests
	levelValuesPerDomain map[utiltas.TopologyDomainId][]string
	infoPerLevel         []infoPerDomain
}

func newTASResourceFlavorSnapshot(levels []string) *TASResourceFlavorSnapshot {
	labelsCopy := slices.Clone(levels)
	snapshot := &TASResourceFlavorSnapshot{
		levelKeys:            labelsCopy,
		capacityPerDomain:    make(map[utiltas.TopologyDomainId]resources.Requests),
		infoPerLevel:         make([]infoPerDomain, len(levels)),
		levelValuesPerDomain: make(map[utiltas.TopologyDomainId][]string),
	}
	return snapshot
}

func (s *TASResourceFlavorSnapshot) initialize() {
	levelCount := len(s.levelKeys)
	lastLevelIdx := levelCount - 1
	for levelIdx := 0; levelIdx < len(s.levelKeys); levelIdx++ {
		s.infoPerLevel[levelIdx] = make(map[utiltas.TopologyDomainId]*info)
	}
	for childId := range s.capacityPerDomain {
		childInfo := &info{
			domainId:     childId,
			childInfoIds: make([]utiltas.TopologyDomainId, 0),
		}
		s.infoPerLevel[lastLevelIdx][childId] = childInfo
		parentFound := false
		var parentInfo *info
		for levelIdx := lastLevelIdx - 1; levelIdx >= 0 && !parentFound; levelIdx-- {
			parentValues := s.levelValuesPerDomain[childId][:levelIdx+1]
			parentId := utiltas.DomainId(parentValues)
			s.memNodeLabels(parentId, parentValues)
			parentInfo, parentFound = s.infoPerLevel[levelIdx][parentId]
			if !parentFound {
				parentInfo = &info{
					domainId:     parentId,
					childInfoIds: make([]utiltas.TopologyDomainId, 0),
				}
				s.infoPerLevel[levelIdx][parentId] = parentInfo
			}
			childInfo.parentDomainId = parentId
			parentInfo.childInfoIds = append(parentInfo.childInfoIds, childId)
			childId = parentId
		}
	}
}

func (tasRf *TASResourceFlavorSnapshot) memNodeLabels(domainId utiltas.TopologyDomainId, values []string) {
	tasRf.levelValuesPerDomain[domainId] = values
}

func (tasRf *TASResourceFlavorSnapshot) addCapacity(domainId utiltas.TopologyDomainId, capacity resources.Requests) {
	if _, found := tasRf.capacityPerDomain[domainId]; !found {
		tasRf.capacityPerDomain[domainId] = resources.Requests{}
	}
	tasRf.capacityPerDomain[domainId].Add(capacity)
}

func (tasRf *TASResourceFlavorSnapshot) addUsage(domainId utiltas.TopologyDomainId, usage resources.Requests) {
	tasRf.capacityPerDomain[domainId].Sub(usage)
}

// Algorithm steps:
// 1. determine pod counts at each topology domain at the lowest level
// 2. bubble up the pod counts to the top level
// 3. select the domain at requested level with count >= requestedCount
// 4. select the lowest-level topology domains corresponding to the requested one until count >= requestedCount
func (tasRf *TASResourceFlavorSnapshot) FindTopologyAssignment(
	topologyRequest *kueue.PodSetTopologyRequest,
	requests resources.Requests,
	count int32) *kueue.TopologyAssignment {
	required := topologyRequest.Required != nil
	levelIdx, found := tasRf.resolveLevelIdx(topologyRequest)
	if !found {
		return nil
	}
	tasRf.fillInCounts(requests)
	fitLevelIdx, fitInfos := tasRf.findLevelWithFitInfos(levelIdx, required, count)
	if len(fitInfos) == 0 {
		return nil
	}
	lowestInfos := tasRf.lowestLevelInfos(fitLevelIdx, fitInfos)
	if len(lowestInfos) == 0 {
		return nil
	}
	sortedLowestInfos := tasRf.sortedInfos(lowestInfos)
	return tasRf.buildAssignment(sortedLowestInfos, count)
}

func (tasRf *TASResourceFlavorSnapshot) resolveLevelIdx(
	topologyRequest *kueue.PodSetTopologyRequest) (int, bool) {
	var levelKey string
	if topologyRequest.Required != nil {
		levelKey = *topologyRequest.Required
	} else if topologyRequest.Preferred != nil {
		levelKey = *topologyRequest.Preferred
	}
	levelIdx := slices.Index(tasRf.levelKeys, levelKey)
	if levelIdx < 0 {
		return levelIdx, false
	}
	return levelIdx, true
}

func (tasRf *TASResourceFlavorSnapshot) findLevelWithFitInfos(levelIdx int, required bool, count int32) (int, []*info) {
	levelInfos := tasRf.infosForLevel(levelIdx)
	if len(levelInfos) == 0 {
		return 0, nil
	}
	sortedInfos := tasRf.sortedInfos(levelInfos)
	topInfo := sortedInfos[0]
	if topInfo.count < count {
		if required {
			return 0, nil
		} else if levelIdx > 0 {
			return tasRf.findLevelWithFitInfos(levelIdx-1, required, count)
		}
		lastIdx := 0
		remainingCount := count - sortedInfos[lastIdx].count
		for remainingCount > 0 && lastIdx < len(sortedInfos)-1 {
			lastIdx++
			remainingCount = remainingCount - sortedInfos[lastIdx].count
		}
		if remainingCount > 0 {
			return 0, nil
		}
		return 0, sortedInfos[:lastIdx+1]
	}
	return levelIdx, []*info{topInfo}
}

func (tasRf *TASResourceFlavorSnapshot) buildAssignment(infos []*info, count int32) *kueue.TopologyAssignment {
	assignment := kueue.TopologyAssignment{
		Domains: make([]kueue.TopologyDomainAssignment, 0),
	}
	remainingCount := count
	for i := 0; i < len(infos) && remainingCount > 0; i++ {
		info := infos[i]
		levels := tasRf.asDomainLevels(info.domainId)
		if info.count >= remainingCount {
			assignment.Domains = append(assignment.Domains, kueue.TopologyDomainAssignment{
				Levels: levels,
				Count:  int(remainingCount),
			})
			remainingCount = 0
		} else if info.count > 0 {
			assignment.Domains = append(assignment.Domains, kueue.TopologyDomainAssignment{
				Levels: levels,
				Count:  int(info.count),
			})
			remainingCount -= info.count
		}
	}
	if remainingCount > 0 {
		return nil
	}
	return &assignment
}

func (tasRf *TASResourceFlavorSnapshot) asDomainLevels(domainId utiltas.TopologyDomainId) []kueue.TopologyDomainAssignmentLevel {
	result := make([]kueue.TopologyDomainAssignmentLevel, len(tasRf.levelKeys))
	for i, labelKey := range tasRf.levelKeys {
		result[i] = kueue.TopologyDomainAssignmentLevel{
			NodeLabel:      labelKey,
			NodeLabelValue: tasRf.levelValuesPerDomain[domainId][i],
		}
	}
	return result
}

func (tasRf *TASResourceFlavorSnapshot) lowestLevelInfos(firstLevelIdx int, infos []*info) []*info {
	result := infos
	for levelIdx := firstLevelIdx; levelIdx < len(tasRf.levelKeys)-1; levelIdx++ {
		result = tasRf.lowerLevelInfos(levelIdx, result)
	}
	return result
}

func (tasRf *TASResourceFlavorSnapshot) lowerLevelInfos(levelIdx int, infos []*info) []*info {
	result := make([]*info, 0, len(infos))
	for _, info := range infos {
		for _, childDomainId := range info.childInfoIds {
			if childDomain := tasRf.infoPerLevel[levelIdx+1][childDomainId]; childDomain != nil {
				result = append(result, childDomain)
			}
		}
	}
	return result
}

func (tasRf *TASResourceFlavorSnapshot) infosForLevel(levelIdx int) []*info {
	infosMap := tasRf.infoPerLevel[levelIdx]
	result := make([]*info, len(infosMap))
	index := 0
	for _, info := range infosMap {
		result[index] = info
		index++
	}
	return result
}

func (tasRf *TASResourceFlavorSnapshot) sortedInfos(infos []*info) []*info {
	result := make([]*info, len(infos))
	copy(result, infos)
	slices.SortFunc(result, func(a, b *info) int {
		if a.count == b.count {
			return strings.Compare(string(a.domainId), string(b.domainId))
		} else if a.count > b.count {
			return -1
		} else {
			return 1
		}
	})
	return result
}

func (tasRf *TASResourceFlavorSnapshot) fillInCounts(requests resources.Requests) {
	tasRf.fillInLeafCounts(requests)
	tasRf.fillInInnerCounts()
}

func (tasRf *TASResourceFlavorSnapshot) fillInInnerCounts() {
	lastLevelIdx := len(tasRf.infoPerLevel) - 1
	for levelIdx := lastLevelIdx - 1; levelIdx >= 0; levelIdx-- {
		for _, info := range tasRf.infoPerLevel[levelIdx] {
			info.count = 0
			for _, childDomainId := range info.childInfoIds {
				info.count += tasRf.infoPerLevel[levelIdx+1][childDomainId].count
			}
		}
	}
}

func (tasRf *TASResourceFlavorSnapshot) fillInLeafCounts(requests resources.Requests) {
	lastLevelIdx := len(tasRf.infoPerLevel) - 1
	for domainId, capacity := range tasRf.capacityPerDomain {
		tasRf.infoPerLevel[lastLevelIdx][domainId].count = requests.CountIn(capacity)
	}
}
