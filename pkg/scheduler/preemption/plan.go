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
	"cmp"
	"context"
	"math"
	"slices"
	"sort"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/workload"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
)

// What a plan leaves to a victim is a count of Pods of its shrinkable PodSet, or
// one of these two. A lower value takes more from the victim.
const (
	// keepNone evicts a victim. It is less than any count: keeping 0 Pods of the
	// shrinkable PodSet leaves the Workload admitted with its other PodSets.
	keepNone int32 = -1
	// keepAll leaves a victim as it was when the search started. It is more than
	// any count.
	keepAll int32 = math.MaxInt32
)

// victim is what a partialPlan memorizes about one candidate.
type victim struct {
	// target is the candidate as the strategy yielded it first. Its WorkloadInfo
	// is the victim as it was when the search started.
	target *Target
	// eviction is the candidate as the search without partial preemption evicts
	// it, nil if that search does not evict the victim. The plan evicts no other
	// victim.
	eviction *Target
	// podSet is the PodSet that partial preemption may shrink, empty when the
	// victim can only be evicted whole.
	podSet kueue.PodSetReference
	// fairFloor is the least that the victim keeps for the strategy to offer
	// it, 0 if the strategy offers it whatever it keeps. It does not hold for a
	// victim that the search without partial preemption evicts.
	fairFloor int32
	// keep is what the plan leaves to the victim right now.
	keep int32
	// current is the instance of the victim that is in the snapshot right now,
	// nil when keep is keepNone.
	current *workload.Info
}

// info is the victim as it was when the search started.
func (v *victim) info() *workload.Info {
	return v.target.WorkloadInfo
}

// canShrink tells whether partial preemption may shrink the victim.
func (v *victim) canShrink() bool {
	return v.podSet != ""
}

// floor is the lowest count that podSet may be shrunk to.
func (v *victim) floor() int32 {
	floor, _ := workload.PreemptionMinRetainedCount(v.info().Obj, v.podSet)
	return max(floor, v.fairFloor)
}

// full is the count of podSet that the victim is charged for.
func (v *victim) full() int32 {
	return podSetRequestCount(v.info(), v.podSet)
}

// inProgress is what the victim keeps once the preemptions that are already in
// progress are done: nothing if it is being evicted, and the count that an
// earlier partial preemption asked for if it has not shrunk to it yet.
func (v *victim) inProgress() int32 {
	if workloadevict.IsEvicted(v.info().Obj) {
		return keepNone
	}
	if count, found := workload.PreemptionRetainedCount(v.info().Obj, v.podSet); found && count < v.full() {
		return count
	}
	return keepAll
}

// shrunk is the least that the victim keeps without a new eviction.
func (v *victim) shrunk() int32 {
	if v.canShrink() {
		return v.floor()
	}
	return v.inProgress()
}

// partialPlan refines the preemption target search of one strategy with partial
// preemption: victims shrink before any victim is evicted.
//
// A strategy offers its candidates one at a time: it removes a candidate from
// the snapshot, yields it, and looks at the snapshot again to pick the next one.
// The search without partial preemption walks the candidates leaving each one
// removed, until the workload fits (see getTargets). The plan adds a walk that
// puts each victim back with the least it keeps without a new eviction, so the
// strategy offers the candidates that follow as if the victim had shrunk. In
// that walk a strategy that judges a candidate by what it gives up offers it
// with the most that it may give up (see usageTakenFrom).
//
// The plan memorizes the victims of both walks, and takes from them in two
// steps:
//
//  1. shrink: every victim keeps the least it can without a new eviction: its
//     floor if it can shrink, and otherwise what an eviction or a shrink that is
//     already in progress leaves it;
//  2. evict: if that is not enough, the targets of the search without partial
//     preemption are evicted on top of the shrinks, in the order in which they
//     were offered, until the workload fits.
//
// Then it gives back what the workload does not need, starting with what
// disrupts the victims most (giveBack), and takes back the shrinks that the
// strategy does not accept (take).
type partialPlan struct {
	strategy PreemptionStrategy
	// victims are the memorized candidates, in the order in which the strategy
	// offered them first.
	victims []*victim
	// byKey indexes the victims by Workload, for a walk to find a victim that
	// was offered before.
	byKey map[workload.Reference]*victim
	// done tells that the plan has nothing to try: no victim can shrink.
	done bool
}

// newPartialPlan returns the plan for the strategy, nil if the PartialPreemption
// feature gate is disabled. A nil plan finds no targets.
func newPartialPlan(strategy PreemptionStrategy) *partialPlan {
	if !features.Enabled(features.PartialPreemption) {
		return nil
	}
	return &partialPlan{strategy: strategy, byKey: make(map[workload.Reference]*victim)}
}

// shrink walks the candidates and returns the targets that make the workload
// fit without a new eviction, the first step of the plan. It returns nil if
// there are none, and leaves the snapshot as it found it.
func (p *partialPlan) shrink(ctx context.Context) []*Target {
	if p == nil {
		return nil
	}
	defer p.restore()
	p.strategy.pCtx.fairFloors = make(map[workload.Reference]int32)
	for candidate := range p.strategy.candidates {
		v := p.memorize(candidate)
		p.set(v, v.shrunk())
	}
	p.strategy.pCtx.fairFloors = nil
	if !slices.ContainsFunc(p.victims, (*victim).canShrink) {
		// The search without partial preemption finds the same targets.
		p.done = true
		return nil
	}
	return p.take(ctx, nil)
}

// usageTakenFrom returns the usage that the walk in progress takes from the
// candidate, for a strategy that offers a candidate only if fair accepts what
// the candidate gives up. fair has to accept every part of a usage that it
// accepts.
//
// The search without partial preemption takes all the usage of a candidate.
// When a partialPlan walks the candidates to shrink them, a candidate that can
// shrink gives up the most that fair accepts, down to its floor, and the plan
// does not shrink it further.
func (c *preemptionCtx) usageTakenFrom(wl *workload.Info, fair func(workload.Usage) bool) workload.Usage {
	target := Target{WorkloadInfo: wl}
	podSet := shrinkablePodSet(&target)
	if c.fairFloors == nil || podSet == "" {
		return wl.Usage()
	}
	takenWith := func(keep int32) workload.Usage {
		target.PartialPreemption = &policy.PartialPreemptionTarget{ReducedInfo: reducedInfoForPartialPreemption(wl, podSet, keep)}
		return target.PreemptedUsage()
	}
	floor, _ := workload.PreemptionMinRetainedCount(wl.Obj, podSet)
	// The lowest count with which fair accepts what the candidate gives up: the
	// floor, or with shrinkToFairFloor any count that takes Pods from the
	// candidate and leaves it no more than a shrink in progress does.
	counts := 1
	if c.shrinkToFairFloor {
		most := podSetRequestCount(wl, podSet) - 1
		if inProgress, shrinking := workload.PreemptionRetainedCount(wl.Obj, podSet); shrinking {
			most = min(most, inProgress)
		}
		counts = int(most - floor + 1)
	}
	found := sort.Search(counts, func(i int) bool { return fair(takenWith(floor + int32(i))) })
	if found >= counts {
		// fair accepts none of these, so it does not accept all the usage either.
		return wl.Usage()
	}
	c.fairFloors[workload.Key(wl.Obj)] = floor + int32(found)
	return takenWith(floor + int32(found))
}

// walkOrder returns a copy of the candidates that the search without partial
// preemption removed from the snapshot, in that order, for evict: the fill-back
// of the search reorders them. It returns nil if the plan has nothing to try.
func (p *partialPlan) walkOrder(walked []*Target) []*Target {
	if p == nil || p.done {
		return nil
	}
	return slices.Clone(walked)
}

// evict returns the targets that make the workload fit by evicting victims on
// top of the shrinks, the second step of the plan. walked are the candidates
// that the search without partial preemption removed from the snapshot, in
// that order. The ones that its fill-back did not put back are its targets,
// which the strategy accepted: the plan evicts no other victim. If evict
// returns targets, the snapshot is as the search found it. Otherwise it is as
// evict found it.
func (p *partialPlan) evict(ctx context.Context, walked []*Target) []*Target {
	if p == nil || p.done {
		return nil
	}
	var evictions []*victim
	for _, candidate := range walked {
		if v := p.memorize(candidate); v.keep == keepNone {
			// The search found it fair to take everything from the victim.
			v.eviction, v.fairFloor = candidate, 0
			evictions = append(evictions, v)
		}
	}
	targets := p.take(ctx, evictions)
	p.restore()
	if targets == nil {
		// The search without partial preemption goes on with its own targets.
		for _, v := range evictions {
			p.set(v, keepNone)
		}
	}
	return targets
}

// take returns the targets that make the workload fit with the given evictions,
// nil if there are none or the strategy does not accept them. A shrink that the
// strategy rejects on its own is taken back first: take tries once more, and
// the victim does not shrink then.
func (p *partialPlan) take(ctx context.Context, evictions []*victim) []*Target {
	targets := p.attempt(ctx, evictions)
	if p.takeBackRejectedShrinks(targets) {
		targets = p.attempt(ctx, evictions)
	}
	if len(targets) == 0 || !p.strategy.accepts(ctx, targets) {
		return nil
	}
	return targets
}

// attempt shrinks every victim, makes the evictions, in order, until the
// workload fits, and gives back what the workload does not need. It returns
// the targets for what is left, nil if the workload does not fit.
func (p *partialPlan) attempt(ctx context.Context, evictions []*victim) []*Target {
	for _, v := range p.victims {
		p.set(v, v.shrunk())
	}
	// With the evictions of the search without partial preemption the workload
	// fits at the latest when all of them are made, as in that search.
	fits := p.fits(ctx)
	for i := 0; !fits && i < len(evictions); i++ {
		p.set(evictions[i], keepNone)
		fits = p.fits(ctx)
	}
	if !fits {
		return nil
	}
	return p.finish(ctx)
}

// takeBackRejectedShrinks checks every shrink among the targets with the
// strategy on its own, in the state that the targets lead to, and makes the
// victims of the rejected ones unable to shrink. A shrink can be rejected
// although the strategy offered its victim: the strategy judged the victim
// before the plan took from others. It reports whether it took any back.
func (p *partialPlan) takeBackRejectedShrinks(targets []*Target) bool {
	if p.strategy.verify == nil {
		return false
	}
	tookBack := false
	for _, target := range targets {
		if target.PartialPreemption == nil {
			continue
		}
		if valid, _ := p.strategy.verify([]*Target{target}); !valid {
			p.byKey[workload.Key(target.WorkloadInfo.Obj)].podSet = ""
			tookBack = true
		}
	}
	return tookBack
}

// memorize returns the victim for the candidate, and records what the search
// left of it in the snapshot: nothing if it removed the candidate, and the
// victim as it was when the search started otherwise.
func (p *partialPlan) memorize(candidate *Target) *victim {
	key := workload.Key(candidate.WorkloadInfo.Obj)
	v, found := p.byKey[key]
	if !found {
		v = &victim{target: candidate, podSet: shrinkablePodSet(candidate)}
		p.byKey[key] = v
		p.victims = append(p.victims, v)
	}
	if fairFloor, judged := p.strategy.pCtx.fairFloors[key]; judged {
		v.fairFloor = fairFloor
	}
	v.keep, v.current = keepNone, nil
	cq := p.strategy.pCtx.snapshot.ClusterQueue(candidate.WorkloadInfo.ClusterQueue)
	if _, inSnapshot := cq.Workloads[key]; inSnapshot {
		v.keep, v.current = keepAll, v.info()
	}
	return v
}

// shrinkablePodSet returns the PodSet of the candidate that partial preemption
// may shrink, empty if it may not shrink the candidate.
func shrinkablePodSet(candidate *Target) kueue.PodSetReference {
	// Victims selected by a PreemptionConfig rule are evicted whole.
	if candidate.Reason == kueue.ConfigurablePreemptionReason {
		return ""
	}
	podSet, _ := workload.PartialPreemptionPodSet(candidate.WorkloadInfo.Obj, candidate.WorkloadInfo.TotalRequests)
	return podSet
}

// finish gives back what the workload does not need, and returns the targets
// for what the plan still takes from the victims.
func (p *partialPlan) finish(ctx context.Context) []*Target {
	p.giveBack(ctx)

	var targets []*Target
	for _, v := range p.victims {
		switch v.keep {
		case keepAll:
		case keepNone:
			// A target of the search without partial preemption, or a victim that
			// is already being evicted.
			targets = append(targets, cmp.Or(v.eviction, v.target))
		default:
			target := *v.target
			target.PartialPreemption = &policy.PartialPreemptionTarget{
				PodSet:        v.podSet,
				RetainedCount: v.keep,
				ReducedInfo:   v.current,
			}
			targets = append(targets, &target)
		}
	}
	return targets
}

// giveBack returns to the victims what the workload does not need, starting
// with what disrupts them most. Each pass goes over the memorized victims in
// the reverse order. A victim with a shrink in progress never gets more than
// that shrink leaves it, unless it is left out altogether.
func (p *partialPlan) giveBack(ctx context.Context) {
	// tryKeep leaves more to the victim if the workload still fits then.
	tryKeep := func(v *victim, keep int32) bool {
		if keep <= v.keep {
			return false
		}
		previous := v.keep
		p.set(v, keep)
		if p.fits(ctx) {
			return true
		}
		p.set(v, previous)
		return false
	}
	// A. A victim that the plan evicted and that can shrink keeps its floor.
	for _, v := range slices.Backward(p.victims) {
		if v.keep == keepNone && v.canShrink() {
			tryKeep(v, v.floor())
		}
	}
	// B. A victim that the plan evicted is not evicted.
	for _, v := range slices.Backward(p.victims) {
		if v.keep == keepNone {
			tryKeep(v, v.inProgress())
		}
	}
	// C. A victim that the plan shrank does not shrink, or else shrinks to the
	// largest count that still fits.
	for _, v := range slices.Backward(p.victims) {
		inProgress := v.inProgress()
		if v.keep == keepNone || v.keep >= inProgress || tryKeep(v, inProgress) {
			continue
		}
		// The workload fits with v.keep and not with tooMany: bisect.
		tooMany := min(v.full(), inProgress)
		for v.keep+1 < tooMany {
			if count := v.keep + (tooMany-v.keep)/2; !tryKeep(v, count) {
				tooMany = count
			}
		}
	}
	// D. A victim whose eviction or shrink was already in progress is left out.
	for _, v := range slices.Backward(p.victims) {
		if v.keep == v.inProgress() {
			tryKeep(v, keepAll)
		}
	}
}

// set makes the snapshot hold keep of the victim.
func (p *partialPlan) set(v *victim, keep int32) {
	if v.keep == keep {
		return
	}
	snapshot := p.strategy.pCtx.snapshot
	if v.current != nil {
		snapshot.RemoveWorkload(v.current)
	}
	switch keep {
	case keepNone:
		v.current = nil
	case keepAll:
		v.current = v.info()
	default:
		v.current = reducedInfoForPartialPreemption(v.info(), v.podSet, keep)
	}
	if v.current != nil {
		snapshot.AddWorkload(v.current)
	}
	v.keep = keep
}

// reducedInfoForPartialPreemption returns a shallow copy of info with the named PodSet's usage scaled
// down to retainedCount. The original is left untouched so the snapshot can restore it unchanged.
func reducedInfoForPartialPreemption(info *workload.Info, podSetName kueue.PodSetReference, retainedCount int32) *workload.Info {
	reduced := *info
	reduced.TotalRequests = make([]workload.PodSetResources, len(info.TotalRequests))
	for idx := range info.TotalRequests {
		psr := info.TotalRequests[idx]
		if psr.Name == podSetName {
			if retainedCount >= 0 && retainedCount < psr.Count {
				psr = *psr.ScaledTo(retainedCount)
			}
		}
		reduced.TotalRequests[idx] = psr
	}
	return &reduced
}

// restore puts every memorized victim back into the snapshot.
func (p *partialPlan) restore() {
	for _, v := range p.victims {
		p.set(v, keepAll)
	}
}

func (p *partialPlan) fits(ctx context.Context) bool {
	return workloadFits(ctx, p.strategy.pCtx, p.strategy.allowBorrowing)
}

func podSetRequestCount(info *workload.Info, podSet kueue.PodSetReference) int32 {
	for i := range info.TotalRequests {
		if info.TotalRequests[i].Name == podSet {
			return info.TotalRequests[i].Count
		}
	}
	return 0
}
