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

package classical

import (
	"iter"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/sets"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/util/priority"
	"sigs.k8s.io/kueue/pkg/workload"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
)

// Band represents one live equivalence tier of classical preemption
// candidates: same segment, same effective priority, and (when
// AdmissionFairSharing applies) at most one LocalQueueFSUsage tier per
// represented ClusterQueue.
type Band struct {
	// Candidates yields the candidates of the band that are eligible in the
	// current snapshot, each with the reason to preempt it. It does not change
	// the snapshot. Every walk starts over from the first candidate of the band.
	Candidates iter.Seq2[*workload.Info, string]
	// IsEligible re-checks whether a candidate that Candidates yielded is
	// still eligible in the current snapshot.
	IsEligible func(*workload.Info) bool
	// EligibilityScope returns the part of the cohort tree whose usage IsEligible
	// depends on, for a candidate that Candidates yielded: its ClusterQueue, or
	// the topmost cohort above it that is below the lowest common ancestor with
	// the preempting ClusterQueue. Removing or shrinking a candidate can change
	// the eligibility of the candidates of the same scope only. It returns nil
	// for a candidate that is eligible whatever the usage.
	EligibilityScope func(*workload.Info) any
}

type bandMember struct {
	wl          *workload.Info
	reason      string
	segment     int
	cq          *schdcache.ClusterQueueSnapshot
	lca         *schdcache.CohortSnapshot
	isSameQueue bool
}

func (m bandMember) isEligible(frs sets.Set[resources.FlavorResource]) bool {
	if _, inSnapshot := m.cq.Workloads[workload.Key(m.wl.Obj)]; !inSnapshot {
		return false
	}
	return m.isSameQueue || !isAnyNodeBelowAncestorWithinNominal(m.cq, m.lca, frs)
}

// eligibilityScope returns the topmost of the nodes that
// isAnyNodeBelowAncestorWithinNominal checks for the member, nil if isEligible
// does not depend on the usage.
func (m bandMember) eligibilityScope() any {
	if m.isSameQueue {
		return nil
	}
	var scope any = m.cq
	for cohort := range m.cq.PathParentToRoot() {
		if cohort == m.lca {
			break
		}
		scope = cohort
	}
	return scope
}

// segmentCursor steps through live-eligible classical candidates in segment order
// without mutating the snapshot.
type segmentCursor interface {
	// Reset moves the cursor back to the first candidate.
	Reset()
	// save returns a function that moves the cursor back to where it is now.
	save() func()
	// peek returns the next candidate without consuming it, together with the
	// function that consumes it.
	peek(borrow bool) (member bandMember, advance func(), ok bool)
}

// bandMatcher decides which consecutive candidates of a segmentCursor form one
// band: the ones in the segment and at the effective priority of the first
// candidate. With AdmissionFairSharing it also admits a single LocalQueue usage
// tier per ClusterQueue, so that a peer from another ClusterQueue cannot chain
// distinct usage tiers of one ClusterQueue into the band.
type bandMatcher struct {
	log           logr.Logger
	enabledAfs    bool
	segment       int
	priority      int64
	usageTierByCQ map[kueue.ClusterQueueReference]float64
}

func newBandMatcher(log logr.Logger, enabledAfs bool, first bandMember) *bandMatcher {
	matcher := &bandMatcher{
		log:        log,
		enabledAfs: enabledAfs,
		segment:    first.segment,
		priority:   priority.EffectivePriority(log, first.wl.Obj),
	}
	matcher.admit(first)
	return matcher
}

// admit reports whether m belongs to the band, and remembers its usage tier if
// it does.
func (b *bandMatcher) admit(m bandMember) bool {
	if m.segment != b.segment || priority.EffectivePriority(b.log, m.wl.Obj) != b.priority {
		return false
	}
	if b.enabledAfs && m.wl.LocalQueueFSUsage != nil {
		if usage, seen := b.usageTierByCQ[m.wl.ClusterQueue]; seen {
			return usage == *m.wl.LocalQueueFSUsage
		}
		if b.usageTierByCQ == nil {
			b.usageTierByCQ = make(map[kueue.ClusterQueueReference]float64)
		}
		b.usageTierByCQ[m.wl.ClusterQueue] = *m.wl.LocalQueueFSUsage
	}
	return true
}

func iterBands(
	cursor segmentCursor,
	log logr.Logger,
	enabledAfs bool,
	frsNeedPreemption sets.Set[resources.FlavorResource],
	borrow bool,
) iter.Seq[Band] {
	return func(yield func(Band) bool) {
		cursor.Reset()
		for {
			head, _, ok := cursor.peek(borrow)
			if !ok {
				return
			}
			restoreBandStart := cursor.save()
			matcher := newBandMatcher(log, enabledAfs, head)
			memberByKey := make(map[workload.Reference]bandMember)

			// walk consumes the candidates that the matcher admits, and yields
			// them unless yieldCand is nil.
			walk := func(yieldCand func(*workload.Info, string) bool) {
				for {
					m, advance, ok := cursor.peek(borrow)
					if !ok || !matcher.admit(m) {
						return
					}
					advance()
					memberByKey[workload.Key(m.wl.Obj)] = m
					if yieldCand != nil && !yieldCand(m.wl, m.reason) {
						return
					}
				}
			}

			band := Band{
				Candidates: func(yieldCand func(*workload.Info, string) bool) {
					restoreBandStart()
					matcher = newBandMatcher(log, enabledAfs, head)
					walk(yieldCand)
				},
				IsEligible: func(wl *workload.Info) bool {
					m, ok := memberByKey[workload.Key(wl.Obj)]
					return ok && m.isEligible(frsNeedPreemption)
				},
				EligibilityScope: func(wl *workload.Info) any {
					if m, ok := memberByKey[workload.Key(wl.Obj)]; ok {
						return m.eligibilityScope()
					}
					return nil
				},
			}
			if !yield(band) {
				return
			}
			// Consume what is left of the band, so that the next one starts
			// after it.
			walk(nil)
		}
	}
}

func hasInProgressPreemption(wl *kueue.Workload) bool {
	return workloadevict.IsEvicted(wl) || workload.HasPendingPreemptionRetainedCount(wl)
}
