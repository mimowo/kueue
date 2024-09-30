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

package tas

import (
	"crypto/sha1"
	"encoding/hex"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
)

type TopologyDomainId string

func DomainId(levelValues []string) TopologyDomainId {
	if len(levelValues) == 0 {
		panic("hash invoked without levelValues")
	}
	h := sha1.New()
	lastLevelIdx := len(levelValues) - 1
	for levelIdx, levelValue := range levelValues {
		h.Write([]byte(levelValue))
		if levelIdx < lastLevelIdx {
			h.Write([]byte("\n"))
		}
	}
	return TopologyDomainId(hex.EncodeToString(h.Sum(nil)))
}

func DomainIdForAssignment(levels []kueue.TopologyDomainAssignmentLevel) TopologyDomainId {
	values := make([]string, len(levels))
	for i, level := range levels {
		values[i] = level.NodeLabelValue
	}
	return DomainId(values)
}

func AsNodeLabels(la []kueue.TopologyDomainAssignmentLevel) map[string]string {
	result := make(map[string]string, len(la))
	for _, level := range la {
		result[level.NodeLabel] = level.NodeLabelValue
	}
	return result
}
