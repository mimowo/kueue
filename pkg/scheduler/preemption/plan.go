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

package preemption

import (
	"context"
	"iter"
	"slices"
	"sort"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/workload"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
)

// plan drives how a single strategy's candidates are consumed and backfilled.
// walk plans each candidate into a target and applies it to the snapshot through
// the yield it is given; fillBack then restores whatever the preemptor no longer
// needs.
type plan interface {
	// walk plans every candidate and yields it.
	walk() iter.Seq[*Target]
	// fillBack restores, in reverse order, every victim the preemptor no longer
	// needs and returns the surviving targets in the order the candidates were
	// considered (decreasing preemption appeal).
	fillBack(ctx context.Context, targets []*Target) []*Target
}

// fullPlan whole-evicts every candidate in a single pass. It is used directly when
// the PartialPreemption feature is off.
type fullPlan struct {
	pCtx           *preemptionCtx
	iterator       candidateIterator
	allowBorrowing bool
}

func (p *fullPlan) walk() iter.Seq[*Target] {
	return func(yield func(*Target) bool) {
		if closer, ok := p.iterator.(interface{ Close() }); ok {
			defer closer.Close()
		}
		yield = policy.YieldFromSnapshot(p.pCtx.snapshot, yield)
		p.iterator.Reset()
		for {
			t := p.iterator.Next(p.allowBorrowing)
			if t == nil {
				break
			}
			// A whole eviction does not depend on the candidate's live snapshot state,
			// so there is no need to look it up; the ClusterQueue is enough to build the
			// target.
			cq := p.pCtx.snapshot.ClusterQueue(t.WorkloadInfo.ClusterQueue)
			if !yield(wholeTarget(t, cq)) {
				return
			}
		}
	}
}

func (p *fullPlan) fillBack(ctx context.Context, targets []*Target) []*Target {
	// In the reverse order, check if any of the workloads can be added back.
	for i := len(targets) - 2; i >= 0; i-- {
		p.pCtx.snapshot.AddWorkload(targets[i].WorkloadInfo)
		if workloadFits(ctx, p.pCtx, p.allowBorrowing) {
			// O(1) deletion: copy the last element into index i and reduce size.
			targets[i] = targets[len(targets)-1]
			targets = targets[:len(targets)-1]
		} else {
			p.pCtx.snapshot.RemoveWorkload(targets[i].WorkloadInfo)
		}
	}
	return targets
}

// partialPlan shrinks shrinkable victims to their floor first and, only if that is
// not enough, escalates to whole evictions. It is used when the PartialPreemption
// feature is on.
type partialPlan struct {
	pCtx           *preemptionCtx
	iterator       candidateIterator
	allowBorrowing bool
	// ordering sorts victims by decreasing preemption appeal; fillBack uses it to
	// restore the least appealing (highest priority) victims first.
	ordering func(a, b *workload.Info) int
}

// walk consumes the candidates in three iterator passes covering four phases, ordered
// from the least to the most disruptive:
//
//   - Pass 1 starts in the evicted phase: the already-evicted prefix (the candidates
//     the generator emits first) is planned as a whole eviction. At the first
//     non-evicted candidate the phase switches to pending, and from there only
//     in-flight partial victims are reused/extended.
//   - Pass 2 is the partial phase: every remaining shrinkable victim is shrunk to its
//     floor. planPreemption returns a partial target only when the live used count is
//     still above the floor, so victims already at their floor (the pending ones) and
//     non-shrinkable ones are skipped.
//   - Pass 3 is the full phase: everything still present is escalated to a whole
//     eviction. currentCandidate skips the evicted prefix because it is no longer in
//     the snapshot.
//
// Every phase reads the victim's live state from the snapshot, so a victim already
// shrunk in an earlier phase is planned against its reduced instance. A shrinkable
// victim therefore appears twice in the yielded stream: once as a pass-2 partial and
// once as a pass-3 whole eviction; fillBack merges the two stages.
func (p *partialPlan) walk() iter.Seq[*Target] {
	return func(yield func(*Target) bool) {
		if closer, ok := p.iterator.(interface{ Close() }); ok {
			defer closer.Close()
		}
		yield = policy.YieldFromSnapshot(p.pCtx.snapshot, yield)
		// Pass 1: the already-evicted prefix as whole evictions, then the in-flight
		// partial victims reused/extended.
		inEvictedPrefix := true
		p.iterator.Reset()
		for {
			t := p.iterator.Next(p.allowBorrowing)
			if t == nil {
				break
			}
			cq, current := currentCandidate(p.pCtx, t.WorkloadInfo)
			if current == nil {
				continue
			}
			if inEvictedPrefix {
				if workloadevict.IsEvicted(t.WorkloadInfo.Obj) {
					if !yield(wholeTarget(t, cq)) {
						return
					}
					continue
				}
				// The evicted prefix is exhausted; the rest of this pass reuses pending victims.
				inEvictedPrefix = false
			}
			// Pass 1 only reuses victims that already have an in-flight partial-preemption
			// reclaim target; other non-evicted candidates are left to pass 2.
			if !workload.HasPendingPreemptionRetainedCount(t.WorkloadInfo.Obj) {
				continue
			}
			target := planPreemption(t, current)
			if target == nil {
				continue
			}
			target.WorkloadCq = cq
			if !yield(target) {
				return
			}
		}
		// Pass 2: partial phase.
		p.iterator.Reset()
		for {
			t := p.iterator.Next(p.allowBorrowing)
			if t == nil {
				break
			}
			cq, current := currentCandidate(p.pCtx, t.WorkloadInfo)
			if current == nil {
				continue
			}
			target := planPreemption(t, current)
			if target == nil {
				continue
			}
			target.WorkloadCq = cq
			if !yield(target) {
				return
			}
		}
		// Pass 3: full phase. Escalate the still-present candidates to whole evictions.
		p.iterator.Reset()
		for {
			t := p.iterator.Next(p.allowBorrowing)
			if t == nil {
				break
			}
			cq, current := currentCandidate(p.pCtx, t.WorkloadInfo)
			if current == nil {
				continue
			}
			if !yield(wholeTarget(t, cq)) {
				return
			}
		}
	}
}

// fillBack merges the staged targets produced by walk and then backfills them in
// reverse (least appealing, i.e. highest priority, victim restored first).
//
// A shrinkable victim can appear twice in the stream: once shrunk (a partial stage)
// and once escalated to a whole eviction. mergeStagedTargets collapses each victim to
// a single target and re-sorts the result by preemption appeal, so backfill does not
// rely on the order the stages happened to be yielded in.
//
// The merged list is walked in reverse. A whole eviction is restored and dropped if the
// preemptor still fits, otherwise kept. A shrinkable victim is handled by
// fillbackPartialPreemption, which tries, in order, a full restore, a floor restore,
// and finally a search for the largest count above the floor that still fits.
func (p *partialPlan) fillBack(ctx context.Context, targets []*Target) []*Target {
	merged := p.mergeStagedTargets(targets)
	for i := len(merged) - 1; i >= 0; i-- {
		t := merged[i]
		// A shrinkable victim, whether staged as a partial shrink or escalated to a whole
		// eviction, is grown back over the full {0} ∪ [min, high] range by
		// fillbackPartialPreemption. The branch keys on whether the victim is shrinkable,
		// not on the staged target kind, so an escalated victim still gets its partial
		// range searched instead of a plain restore-or-keep.
		if podSet, ok := workload.PartialPreemptionPodSet(t.WorkloadInfo.Obj, t.WorkloadInfo.TotalRequests); ok {
			if fillbackPartialPreemption(ctx, p.pCtx, t, podSet, p.allowBorrowing) {
				// O(1) deletion: copy the last element into index i and reduce size.
				merged[i] = merged[len(merged)-1]
				merged = merged[:len(merged)-1]
			}
			continue
		}
		// Whole eviction: restore it and drop it if the preemptor still fits.
		p.pCtx.snapshot.AddWorkload(t.WorkloadInfo)
		if workloadFits(ctx, p.pCtx, p.allowBorrowing) {
			merged[i] = merged[len(merged)-1]
			merged = merged[:len(merged)-1]
			continue
		}
		p.pCtx.snapshot.RemoveWorkload(t.WorkloadInfo)
	}
	return merged
}

// mergeStagedTargets collapses the staged targets to one target per victim and sorts
// them by decreasing preemption appeal. A shrinkable victim that both shrank (pass 2)
// and later escalated to a whole eviction (pass 3) appears twice; the first staged target
// is kept. Both stages carry the same full WorkloadInfo, and fillbackPartialPreemption
// reconstructs the victim's walk-end state from the live snapshot (see wasWholeEvicted)
// rather than from the target, so the outcome is identical whichever stage is kept. The
// result is sorted by p.ordering so fillBack can restore the least appealing victims first
// regardless of the yielded stage order.
func (p *partialPlan) mergeStagedTargets(targets []*Target) []*Target {
	byKey := make(map[workload.Reference]*Target, len(targets))
	for _, t := range targets {
		key := workload.Key(t.WorkloadInfo.Obj)
		if _, seen := byKey[key]; !seen {
			byKey[key] = t
		}
	}
	merged := make([]*Target, 0, len(byKey))
	for _, t := range byKey {
		merged = append(merged, t)
	}
	slices.SortStableFunc(merged, func(a, b *Target) int {
		return p.ordering(a.WorkloadInfo, b.WorkloadInfo)
	})
	return merged
}

// fillbackPartialPreemption grows a shrinkable victim t back toward its full size. It
// tries, in order:
//
//	(a) a full restore — drop the target entirely if the preemptor still fits with the
//	    victim completely back;
//	(b) if the full restore didn't apply or didn't fit, a restore to minRetainCount
//	    (the floor): if that fits, there's no need to fall back further. If even
//	    minRetainCount doesn't fit, the outcome depends on how walk left the victim,
//	    captured as wasWholeEvicted before the snapshot is mutated: a victim already
//	    whole-evicted during walk settles at 0, which walk already proved fits; a
//	    victim that was only shrunk to minRetainCount (never escalated) keeps that
//	    shrink instead of escalating to a whole eviction here — its PartialPreemption
//	    already reflects minRetainCount, set by pass 1/2's planPreemption, so there is
//	    nothing left to update;
//	(c) if minRetainCount fits, a search in (minRetainCount, min(full, high)) for the
//	    largest count that still fits (high caps the regrowth at an already in-flight
//	    retained count, if one is pending for this victim).
//
// All arithmetic uses the original full WorkloadInfo so Revert/Apply restore the
// victim's whole quota.
func fillbackPartialPreemption(ctx context.Context, preemptionCtx *preemptionCtx, t *Target, podSet kueue.PodSetReference, allowBorrowing bool) (dropped bool) {
	minRetainCount, _ := workload.PreemptionMinRetainedCount(t.WorkloadInfo.Obj, podSet)
	full := podSetRequestCount(t.WorkloadInfo, podSet)
	high := full
	if pending, has := workload.PreemptionRetainedCount(t.WorkloadInfo.Obj, podSet); has && pending < high {
		high = pending
	}

	snapshot := preemptionCtx.snapshot
	cq := snapshot.ClusterQueue(t.WorkloadInfo.ClusterQueue)
	key := workload.Key(t.WorkloadInfo.Obj)
	wasWholeEvicted := cq.Workloads[key] == nil

	// setTo rewrites the victim's snapshot instance to hold count.
	setTo := func(count int32) {
		if current := cq.Workloads[key]; current != nil {
			snapshot.RemoveWorkload(current)
		}
		switch {
		case count >= full:
			snapshot.AddWorkload(t.WorkloadInfo)
		case count > 0:
			snapshot.AddWorkload(reducedInfoForPartialPreemption(t.WorkloadInfo, podSet, count))
		}
	}
	// restorePartial sets the snapshot and records the partial target at count.
	restorePartial := func(count int32) {
		setTo(count)
		t.PartialPreemption = &policy.PartialPreemptionTarget{
			PodSet:        podSet,
			RetainedCount: count,
			ReducedInfo:   reducedInfoForPartialPreemption(t.WorkloadInfo, podSet, count),
		}
	}

	// (a) Full restore.
	if high >= full {
		setTo(full)
		if workloadFits(ctx, preemptionCtx, allowBorrowing) {
			return true
		}
	}

	// (b) minRetainCount (floor) restore.
	setTo(minRetainCount)
	if !workloadFits(ctx, preemptionCtx, allowBorrowing) {
		if wasWholeEvicted {
			// walk already proved the preemptor fits with the victim fully gone.
			setTo(0)
			t.PartialPreemption = nil
		}
		// Otherwise the victim was only ever shrunk to minRetainCount (never
		// escalated to a whole eviction), and t.PartialPreemption already holds that
		// shrink from pass 1/2's planPreemption — the snapshot (just set above) and
		// the target already agree, nothing left to update.
		return false
	}

	// (c) minRetainCount fits: search (minRetainCount, min(full, high)) for the
	// largest count that still fits. A smaller count frees more room, so sort.Search
	// finds the largest fitting one.
	best := minRetainCount
	if band := min(high, full-1); band > minRetainCount {
		n := int(band - minRetainCount)
		k := sort.Search(n, func(k int) bool {
			setTo(band - int32(k))
			return workloadFits(ctx, preemptionCtx, allowBorrowing)
		})
		if k < n {
			best = band - int32(k)
		}
	}
	restorePartial(best)
	return false
}

func podSetRequestCount(info *workload.Info, podSet kueue.PodSetReference) int32 {
	for i := range info.TotalRequests {
		if info.TotalRequests[i].Name == podSet {
			return info.TotalRequests[i].Count
		}
	}
	return 0
}

// newPlan selects the plan for a preemption attempt: partial when the feature is
// enabled, whole eviction otherwise. ordering sorts victims by decreasing preemption
// appeal and is used by partialPlan.fillBack to merge the staged targets.
func newPlan(pCtx *preemptionCtx, iterator candidateIterator, allowBorrowing bool, ordering func(a, b *workload.Info) int) plan {
	if features.Enabled(features.PartialPreemption) {
		return &partialPlan{pCtx: pCtx, iterator: iterator, allowBorrowing: allowBorrowing, ordering: ordering}
	}
	return &fullPlan{pCtx: pCtx, iterator: iterator, allowBorrowing: allowBorrowing}
}

// currentCandidate returns the candidate's ClusterQueue snapshot and its live
// instance in that snapshot.
func currentCandidate(pCtx *preemptionCtx, candidateWl *workload.Info) (*schdcache.ClusterQueueSnapshot, *workload.Info) {
	cq := pCtx.snapshot.ClusterQueue(candidateWl.ClusterQueue)
	if cq == nil {
		return nil, nil
	}
	return cq, cq.Workloads[workload.Key(candidateWl.Obj)]
}

func wholeTarget(seed *Target, cq *schdcache.ClusterQueueSnapshot) *Target {
	seed.WorkloadCq = cq
	return seed
}

// planPreemption plans a shrink of seed's candidate's shrinkable PodSet to its floor, or
// returns nil when no shrink applies: the candidate is not partial-eligible, or its live
// used count is already at or below the floor. It leaves the snapshot unchanged.
func planPreemption(seed *Target, current *workload.Info) *Target {
	candidate := seed.WorkloadInfo
	podSet, ok := workload.PartialPreemptionPodSet(candidate.Obj, candidate.TotalRequests)
	if !ok {
		return nil
	}

	minRetainedCount, _ := workload.PreemptionMinRetainedCount(candidate.Obj, podSet)
	usedCount := podSetRequestCount(current, podSet)
	if usedCount <= minRetainedCount {
		return nil
	}

	seed.PartialPreemption = &policy.PartialPreemptionTarget{
		PodSet: podSet, RetainedCount: minRetainedCount,
		ReducedInfo: reducedInfoForPartialPreemption(candidate, podSet, minRetainedCount),
	}
	return seed
}
