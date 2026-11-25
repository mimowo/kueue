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
	"fmt"
	"math"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/metrics"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/workload"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
	"sigs.k8s.io/kueue/pkg/workload/patching"
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
	// eviction is the candidate as the eviction phase yielded it, nil if the
	// victim was not escalated to eviction. Its WorkloadInfo is always the
	// victim as it was when the search started.
	eviction *Target
	// podSet is the PodSet that partial preemption may shrink, empty when the
	// victim can only be evicted whole.
	podSet kueue.PodSetReference
	// keep is what the plan leaves to the victim right now.
	keep int32
	// current is the instance of the victim that is in the snapshot right now,
	// nil when keep is keepNone.
	current *workload.Info
	// inBand reports whether the victim has been assigned to a band for
	// reverse-band fill-back.
	inBand bool
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
	return floor
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
		return min(v.inProgress(), v.floor())
	}
	return v.inProgress()
}

type peerState struct {
	victim  *victim
	preKeep int32
}

// bandPeers records the victims touched in a single band together with the
// keep state each had before entering that band.
type bandPeers struct {
	list   []peerState
	preByV map[*victim]int32
}

func newBandPeers() *bandPeers {
	return &bandPeers{preByV: make(map[*victim]int32)}
}

func (bp *bandPeers) record(v *victim, preKeep int32, bandVictims *[]*victim) {
	if _, seen := bp.preByV[v]; seen {
		return
	}
	bp.preByV[v] = preKeep
	bp.list = append(bp.list, peerState{victim: v, preKeep: preKeep})
	if !v.inBand {
		v.inBand = true
		*bandVictims = append(*bandVictims, v)
	}
}

func (bp *bandPeers) preKeep(v *victim) int32 {
	if keep, ok := bp.preByV[v]; ok {
		return keep
	}
	return keepAll
}

func (bp *bandPeers) revert(p *partialPlan) {
	for _, ps := range bp.list {
		p.setKeep(ps.victim, ps.preKeep)
	}
}

// partialPlan refines the preemption target search of one strategy with partial
// preemption across the strategy's live candidate bands. A strategy that
// defines no bands is searched as one band that holds all its candidates.
//
// Within each band, victims shrink first (shrinkBand). If that is not enough,
// victims of the band are evicted on top of the shrinks (evictBand). Once the
// workload fits, fillBack returns what the workload does not need, restoring
// later (higher-priority) bands before earlier ones.
type partialPlan struct {
	strategy PreemptionStrategy
	// victims are the memorized candidates, in the order in which the strategy
	// offered them first.
	victims []*victim
	// bands partitions victims by the first band that touched them, in
	// decreasing preemption appeal order, for reverse-band fill-back.
	bands [][]*victim
	// byKey indexes the victims by Workload, for a walk to find a victim that
	// was offered before.
	byKey map[workload.Reference]*victim
}

// newPartialPlan returns the plan for the strategy, nil if the PartialPreemption
// feature gate is disabled.
func newPartialPlan(strategy PreemptionStrategy) *partialPlan {
	if !features.Enabled(features.PartialPreemption) {
		return nil
	}
	return &partialPlan{strategy: strategy, byKey: make(map[workload.Reference]*victim)}
}

// search walks the strategy's live bands in order and returns the targets
// that make the workload fit, always restoring the snapshot before returning.
// When fallback is true, the caller runs the whole-eviction search instead.
func (p *partialPlan) search(ctx context.Context) ([]*Target, bool) {
	defer p.restore()

	if p.creditInProgress(ctx) {
		if targets := p.buildTargets(ctx); targets != nil {
			return targets, false
		}
		return nil, true
	}

	var lastWholeEvictions []*victim
	lastBandIdx := -1
	fits := false
	for band := range p.strategy.candidateBands() {
		bandIdx := len(p.bands)
		lastBandIdx = bandIdx
		p.bands = append(p.bands, nil)
		peers := newBandPeers()

		if p.shrinkBand(ctx, bandIdx, band, peers) {
			saved := p.snapshotKeeps()
			if targets := p.buildTargets(ctx); targets != nil {
				return targets, false
			}
			p.restoreKeeps(saved)
		}

		fits, lastWholeEvictions = p.evictBand(ctx, bandIdx, band, peers)
		if fits {
			break
		}
	}
	if !fits {
		return nil, p.hasShrunkVictim()
	}
	if targets := p.buildTargets(ctx); targets != nil {
		return targets, false
	}
	if lastWholeEvictions != nil && lastBandIdx == 0 {
		return p.acceptWholeEvictions(ctx, lastBandIdx, lastWholeEvictions), false
	}
	return nil, true
}

// creditInProgress applies already-committed evictions and pending shrinks
// across the strategy before opening the first band.
func (p *partialPlan) creditInProgress(ctx context.Context) bool {
	if p.strategy.inProgressCandidates == nil {
		return false
	}
	credited := false
	for candidate := range p.strategy.inProgressCandidates {
		v, _ := p.observe(candidate)
		keep := v.inProgress()
		p.setKeep(v, keep)
		if keep < keepAll {
			credited = true
		}
	}
	return credited && p.fits(ctx)
}

// shrinkBand runs the shrink phase for one band, putting each yielded peer
// at its shrunk count, and reports whether the workload now fits.
func (p *partialPlan) shrinkBand(ctx context.Context, bandIdx int, band candidateBand, peers *bandPeers) bool {
	if band.shrinkCandidates == nil {
		return false
	}
	p.strategy.pCtx.shrinkingPlan = p
	defer func() { p.strategy.pCtx.shrinkingPlan = nil }()

	hasShrinkable := false
	for candidate := range band.shrinkCandidates {
		v, prevKeep := p.observe(candidate)
		peers.record(v, prevKeep, &p.bands[bandIdx])
		if v.canShrink() {
			hasShrinkable = true
		}
		p.setKeep(v, v.shrunk())
	}
	return hasShrinkable && p.fits(ctx)
}

// evictBand escalates one band to whole evictions, chosen the way the search
// without partial preemption chooses them. It undoes the shrinks of the band,
// removes candidates in walk order until the workload fits, and drops the
// evictions that the workload does not need. It then applies the remaining
// evictions on top of the shrinks of the band, in walk order, until the
// workload fits. It reports whether the workload fits, and returns the
// evictions that are needed without any shrink.
func (p *partialPlan) evictBand(ctx context.Context, bandIdx int, band candidateBand, peers *bandPeers) (bool, []*victim) {
	if band.evictCandidates == nil {
		return false, nil
	}
	peers.revert(p)

	var evicted []*victim
	fits := false
	for candidate := range band.evictCandidates {
		// The walk removed the candidate from the snapshot, which evicts it.
		v, prevKeep := p.observe(candidate)
		peers.record(v, prevKeep, &p.bands[bandIdx])
		evCopy := *candidate
		evCopy.WorkloadInfo = v.info()
		v.eviction = &evCopy
		if prevKeep != keepNone {
			evicted = append(evicted, v)
		}
		if p.fits(ctx) {
			fits = true
			break
		}
	}
	if !fits {
		return false, nil
	}
	evicted = p.filterNeededEvictions(ctx, peers, evicted)
	return p.overlayEvictionsOnShrinks(ctx, band, peers, evicted), evicted
}

// filterNeededEvictions drops earlier evicted peers that are not needed for
// whole eviction alone to fit (matching fillBackWorkloads).
func (p *partialPlan) filterNeededEvictions(ctx context.Context, peers *bandPeers, evicted []*victim) []*victim {
	if len(evicted) <= 1 {
		return evicted
	}
	for _, ps := range peers.list {
		if ps.victim.keep != keepNone {
			p.setKeep(ps.victim, ps.preKeep)
		}
	}
	surviving := slices.Clone(evicted)
	for i := len(surviving) - 2; i >= 0; i-- {
		v := surviving[i]
		p.setKeep(v, peers.preKeep(v))
		if p.fits(ctx) {
			surviving = slices.Delete(surviving, i, i+1)
		} else {
			p.setKeep(v, keepNone)
		}
	}
	return surviving
}

// overlayEvictionsOnShrinks adds the evictions, in walk order, to the shrinks
// of the peers until the workload fits, and reports whether it does. A band
// that re-checks eligibility shrinks a peer only while the peer is eligible,
// which an eviction can change for the peers that share the eligibility scope
// of the evicted victim.
func (p *partialPlan) overlayEvictionsOnShrinks(
	ctx context.Context,
	band candidateBand,
	peers *bandPeers,
	evictions []*victim,
) bool {
	if len(evictions) == 0 {
		return p.fits(ctx)
	}
	peers.revert(p)
	p.shrinkEligible(band, peers.list)
	scopes := groupByEligibilityScope(band, peers.list)
	for _, v := range evictions {
		p.setKeep(v, keepNone)
		if band.eligibilityScope != nil {
			p.reshrink(band, scopes[band.eligibilityScope(v.target)], v)
		}
		if p.fits(ctx) {
			return true
		}
	}
	return false
}

// shrinkEligible shrinks the peers in order, each of them only if the band
// finds it eligible when its turn comes.
func (p *partialPlan) shrinkEligible(band candidateBand, peers []peerState) {
	for _, ps := range peers {
		v := ps.victim
		// An evicted victim keeps nothing, which is less than any count.
		if v.shrunk() >= v.keep {
			continue
		}
		if band.isEligible != nil && !band.isEligible(v.target) {
			continue
		}
		p.setKeep(v, v.shrunk())
	}
}

// peerScope holds, in walk order, the peers of a band that share an
// eligibility scope.
type peerScope struct {
	peers []peerState
	// severalClusterQueues tells that the peers are not all in one ClusterQueue.
	severalClusterQueues bool
}

// groupByEligibilityScope returns the peers by eligibility scope, without the
// peers that are eligible whatever the usage. It returns nil for a band that
// does not re-check eligibility.
func groupByEligibilityScope(band candidateBand, peers []peerState) map[any]*peerScope {
	if band.eligibilityScope == nil {
		return nil
	}
	scopes := make(map[any]*peerScope)
	for _, ps := range peers {
		key := band.eligibilityScope(ps.victim.target)
		if key == nil {
			continue
		}
		scope := scopes[key]
		if scope == nil {
			scope = &peerScope{}
			scopes[key] = scope
		} else if scope.peers[0].victim.info().ClusterQueue != ps.victim.info().ClusterQueue {
			scope.severalClusterQueues = true
		}
		scope.peers = append(scope.peers, ps)
	}
	return scopes
}

// reshrink updates the shrinks of a scope after the eviction of one of its
// peers, to the ones that shrinkEligible leaves with that eviction.
func (p *partialPlan) reshrink(band candidateBand, scope *peerScope, evicted *victim) {
	if scope == nil {
		return
	}
	// The eviction leaves less usage to the ClusterQueue of the evicted victim
	// and to the cohorts above it. A shrink in that ClusterQueue that still
	// stands once the later shrinks are undone shows that the peers before it
	// stay as they are. So undo the shrinks latest first down to that one, and
	// shrink again from there.
	first := 0
	for i, ps := range slices.Backward(scope.peers) {
		v := ps.victim
		if v.keep == keepNone || v.keep == ps.preKeep {
			continue
		}
		p.setKeep(v, ps.preKeep)
		if v.info().ClusterQueue == evicted.info().ClusterQueue && band.isEligible(v.target) {
			first = i
			break
		}
	}
	p.shrinkEligible(band, scope.peers[first:])
	if !scope.severalClusterQueues {
		// The peers of one ClusterQueue are eligible up to the same point of
		// the walk, and an eviction only moves that point earlier: the peers
		// after it do not shrink again.
		scope.peers = scope.peers[:first+1]
	}
}

// acceptWholeEvictions evaluates pure whole evictions for a single-band
// search when strategy verification rejected shrunk targets.
func (p *partialPlan) acceptWholeEvictions(ctx context.Context, bandIdx int, wholeEvictions []*victim) []*Target {
	for _, v := range p.bands[bandIdx] {
		if v.inProgress() == keepNone {
			p.setKeep(v, keepNone)
		} else {
			p.setKeep(v, keepAll)
		}
	}
	for _, v := range wholeEvictions {
		p.setKeep(v, keepNone)
	}
	targets := p.collectTargets()
	if len(targets) == 0 || !p.strategy.accepts(ctx, targets) {
		return nil
	}
	return targets
}

func (p *partialPlan) hasShrunkVictim() bool {
	return slices.ContainsFunc(p.victims, func(v *victim) bool {
		return v.keep != keepNone && v.keep != keepAll
	})
}

func (p *partialPlan) snapshotKeeps() []peerState {
	saved := make([]peerState, len(p.victims))
	for i, v := range p.victims {
		saved[i] = peerState{victim: v, preKeep: v.keep}
	}
	return saved
}

func (p *partialPlan) restoreKeeps(saved []peerState) {
	for _, s := range saved {
		p.setKeep(s.victim, s.preKeep)
	}
}

// observe returns the victim for the candidate that a walk has just removed
// from the snapshot, and what the plan left to the victim before that.
func (p *partialPlan) observe(candidate *Target) (*victim, int32) {
	key := workload.Key(candidate.WorkloadInfo.Obj)
	v, found := p.byKey[key]
	prevKeep := keepAll
	if !found {
		v = &victim{target: candidate, podSet: shrinkablePodSet(candidate)}
		p.byKey[key] = v
		p.victims = append(p.victims, v)
	} else {
		prevKeep = v.keep
	}
	v.keep, v.current = keepNone, nil
	return v, prevKeep
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

// buildTargets fills back what the workload does not need, and returns the targets
// for what the plan still takes from the victims. It returns nil if there are
// none, or if the strategy does not accept them: it verifies them like the
// targets of the search without partial preemption, so Fair Sharing verifies
// them only with the FairSharingVerifyFinalTargets feature gate.
func (p *partialPlan) buildTargets(ctx context.Context) []*Target {
	p.fillBack(ctx)

	targets := p.collectTargets()
	if len(targets) == 0 || !p.strategy.accepts(ctx, targets) {
		return nil
	}
	return targets
}

func (p *partialPlan) collectTargets() []*Target {
	var targets []*Target
	for _, v := range p.victims {
		switch v.keep {
		case keepAll:
		case keepNone:
			// A target of the eviction phase, or a victim that is already being evicted.
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

// fillBack returns to the victims what the workload does not need, band by
// band from the highest-priority (last touched) band down to the lowest.
// Within each band, phases A/B/C run in reverse observation order, starting
// with what disrupts victims most. Finally, phase D drops unneeded in-progress
// reductions in reverse order.
func (p *partialPlan) fillBack(ctx context.Context) {
	// tryKeep leaves more to the victim if the workload still fits then.
	tryKeep := func(v *victim, keep int32) bool {
		if keep <= v.keep {
			return false
		}
		previous := v.keep
		p.setKeep(v, keep)
		if p.fits(ctx) {
			return true
		}
		p.setKeep(v, previous)
		return false
	}
	for _, band := range slices.Backward(p.bands) {
		p.fillBackBand(band, tryKeep)
	}
	// D. A victim whose eviction or shrink was already in progress is left out.
	for _, v := range slices.Backward(p.victims) {
		if v.keep == v.inProgress() {
			tryKeep(v, keepAll)
		}
	}
}

func (p *partialPlan) fillBackBand(members []*victim, tryKeep func(*victim, int32) bool) {
	// A. A victim that the plan evicted and that can shrink keeps its floor.
	for _, v := range slices.Backward(members) {
		if v.keep == keepNone && v.canShrink() {
			tryKeep(v, v.floor())
		}
	}
	// B. A victim that the plan evicted is not evicted.
	for _, v := range slices.Backward(members) {
		if v.keep == keepNone {
			tryKeep(v, v.inProgress())
		}
	}
	// C. A victim that the plan shrank does not shrink, or else shrinks to the
	// largest count that still fits.
	for _, v := range slices.Backward(members) {
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
}

// setKeep makes the snapshot hold keep of the victim.
func (p *partialPlan) setKeep(v *victim, keep int32) {
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
		p.setKeep(v, keepAll)
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

// partiallyPreempt requests a scale-down; quota remains charged until the excess Pods terminate.
func (p *Preemptor) partiallyPreempt(ctx context.Context, preemptor *workload.Info, target *Target) error {
	pp := target.PartialPreemption
	wlCopy := target.WorkloadInfo.Obj.DeepCopy()
	message := fmt.Sprintf("Requested scale-down of PodSet %q to %d to accommodate workload %s",
		pp.PodSet, pp.RetainedCount, klog.KObj(preemptor.Obj))
	if err := patching.PatchAdmissionStatus(ctx, p.client, wlCopy, p.clock, func(wl *kueue.Workload) (bool, error) {
		setRetainedCount(wl, pp.PodSet, pp.RetainedCount)
		workload.SetPartiallyPreemptedCondition(wl, p.clock.Now(), true, target.Reason, message)
		return true, nil
	}, patching.WithLooseOnApply()); err != nil {
		return err
	}
	metrics.ReportPartialPreemption(preemptor.ClusterQueue, target.Reason, p.customLabels.CQGet(preemptor.ClusterQueue), p.roleTracker)
	ctrl.LoggerFrom(ctx).V(3).Info("Partial preemption: requested scale-down",
		"targetWorkload", klog.KObj(target.WorkloadInfo.Obj), "podSet", pp.PodSet,
		"RetainedCount", pp.RetainedCount, "preemptingWorkload", klog.KObj(preemptor.Obj))
	p.recorder.Eventf(target.WorkloadInfo.Obj, nil, corev1.EventTypeNormal, "PartiallyPreempted", "PartiallyPreempted", "%s", message)
	return nil
}

// setRetainedCount records count as the named PodSet's requested partial-preemption target,
// reporting whether the value changed.
func setRetainedCount(wl *kueue.Workload, podSetName kueue.PodSetReference, count int32) bool {
	if wl.Status.Admission == nil {
		return false
	}
	for i := range wl.Status.Admission.PodSetAssignments {
		psa := &wl.Status.Admission.PodSetAssignments[i]
		if psa.Name == podSetName {
			if psa.PartialPreemption != nil && psa.PartialPreemption.RetainedCount != nil && *psa.PartialPreemption.RetainedCount <= count {
				return false
			}
			psa.PartialPreemption = &kueue.PodSetPartialPreemptionStatus{RetainedCount: new(count)}
			return true
		}
	}
	return false
}
