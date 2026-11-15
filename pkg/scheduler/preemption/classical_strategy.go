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

	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/classical"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
)

type candidateIterator interface {
	Reset()
	Next(borrow bool) *Target
}

// classicalPreemptionStrategy implements a heuristic to find a minimal set of Workloads
// to preempt.
// The heuristic first removes candidates, in the input order, while their
// ClusterQueues are still borrowing resources and while the incoming Workload
// doesn't fit in the quota.
func classicalPreemptionStrategy(ctx context.Context, preemptor *Preemptor, preemptionCtx *preemptionCtx) iter.Seq[PreemptionStrategy] {
	log := log.FromContext(ctx)
	hierarchicalReclaimCtx := &classical.HierarchicalPreemptionCtx{
		Log:               log,
		Wl:                preemptionCtx.preemptor.Obj,
		Cq:                preemptionCtx.preemptorCQ,
		FrsNeedPreemption: preemptionCtx.frsNeedPreemption,
		Requests:          preemptionCtx.workloadUsage.Quota.Assigned,
		WorkloadOrdering:  preemptor.workloadOrdering,
	}
	candidatesGenerator := classical.NewCandidateIterator(
		hierarchicalReclaimCtx,
		preemptor.enabledAfs,
		preemptionCtx.frsNeedPreemption,
		preemptionCtx.snapshot,
		preemptor.clock,
		policy.CandidatesOrdering,
	)
	var attemptPossibleOpts []preemptionAttemptOpts
	borrowWithinCohortForbidden, _ := classical.IsBorrowingWithinCohortForbidden(preemptionCtx.preemptorCQ)
	// We have three types of candidates:
	// 1. Hierarchy candidates. Candidates over which the incoming workload has a
	//    hierarchical advantage (it is closer to the quota used by the candidate).
	//    We can preempt such candidates regardless of their priority.
	// 2. Priority candidates. Candidates over which there is no hiearchical advantage
	//    but the possibility to preempt is determined based on priorities.
	//    We respect the BorrowWithinCohort configuration only for these candidates.
	// 3. Same queue candidates.
	// We can only preempt a priority candidate with priority > MaxPriorityThreshold
	// if the target CQ is not borrowing (by the definition of the MaxPriorityThreshold).
	// We sometimes need to consider both options allowBorrowing = true and false
	// (because with false we have more candidates but cannot use borrowing).
	// The order in which the options are considered is arbitrary and the condition
	// in which we try allowBorrowing=false before true is to keep compatibility with
	// previous versions.
	switch {
	case candidatesGenerator.NoCandidateFromOtherQueues() || (borrowWithinCohortForbidden && !queueUnderNominalInResourcesNeedingPreemption(preemptionCtx)):
		attemptPossibleOpts = []preemptionAttemptOpts{{true}}
	case borrowWithinCohortForbidden && candidatesGenerator.NoCandidateForHierarchicalReclaim():
		attemptPossibleOpts = []preemptionAttemptOpts{{false}, {true}}
	default:
		attemptPossibleOpts = []preemptionAttemptOpts{{true}, {false}}
	}

	return func(yieldStrategy func(PreemptionStrategy) bool) {
		for _, opts := range attemptPossibleOpts {
			allowBorrowing := opts.borrowing
			candidateIter := func(yieldCandidate func(*Target) bool) {
				cont := iterateOverCandidates(ctx, preemptionCtx, candidatesGenerator, allowBorrowing, yieldCandidate)
				if cont && features.Enabled(features.ConfigurablePreemptions) {
					preemptionCtx.configurableEvaluator.FindCandidates(
						preemptionCtx.snapshot,
						preemptionCtx.frsNeedPreemption,
						func() bool { return workloadQuotaFits(preemptionCtx, allowBorrowing) },
						yieldCandidate,
					)
				}
			}
			classicalIter := &classicalCandidateIterator{
				preemptionCtx:  preemptionCtx,
				classical:      candidatesGenerator,
				allowBorrowing: allowBorrowing,
			}
			pl := newPlan(preemptionCtx, classicalIter, allowBorrowing, preemptor.candidatesOrdering(log, preemptionCtx.preemptorCQ.Name))
			if !yieldStrategy(newPreemptionStrategy(candidateIter, allowBorrowing, preemptionCtx, pl)) {
				return
			}
		}
	}
}

// classicalCandidateIterator feeds plan.walk candidates without applying them to the
// snapshot: Next returns the classical candidate as-is (PartialPreemption stays nil,
// the workload is still fully in the snapshot). plan.walk's own YieldFromSnapshot wrap
// is what actually mutates the snapshot, and only after walk has decided whether to
// shrink the candidate or whole-evict it. Applying here instead would make that
// decision impossible: partialPlan.walk reads each candidate's live snapshot state to
// make it.
//
// ConfigurablePreemptions candidates are the opposite: FindCandidates applies each one
// internally before Next returns it (its own trigger-escalation logic needs the
// snapshot updated between candidates). plan.walk's wrap re-applying an
// already-removed workload is a no-op (Target.Apply skips it), not a bug.
type classicalCandidateIterator struct {
	preemptionCtx  *preemptionCtx
	classical      classical.CandidateIterator
	allowBorrowing bool

	configurableNext func() (*Target, bool)
	configurableStop func()
}

func (c *classicalCandidateIterator) Reset() {
	c.classical.Reset()
}

func (c *classicalCandidateIterator) Next(borrow bool) *Target {
	if candidateWl, reason := c.classical.Next(borrow); candidateWl != nil {
		return &Target{
			WorkloadInfo: candidateWl,
			Reason:       reason,
			WorkloadCq:   c.preemptionCtx.snapshot.ClusterQueue(candidateWl.ClusterQueue),
		}
	}
	if !features.Enabled(features.ConfigurablePreemptions) {
		return nil
	}
	// TODO: So far, we have only traversed the candidate set of ConfigurablePreemptions once.
	// Therefore, enabling PartialPreemption and ConfigurablePreemptions simultaneously is not
	// supported for the time being.
	if c.configurableNext == nil {
		c.configurableNext, c.configurableStop = iter.Pull(func(yield func(*Target) bool) {
			c.preemptionCtx.configurableEvaluator.FindCandidates(
				c.preemptionCtx.snapshot,
				c.preemptionCtx.frsNeedPreemption,
				func() bool { return workloadQuotaFits(c.preemptionCtx, c.allowBorrowing) },
				yield,
			)
		})
	}
	t, ok := c.configurableNext()
	if !ok {
		return nil
	}
	return t
}

func (c *classicalCandidateIterator) Close() {
	if c.configurableStop != nil {
		c.configurableStop()
	}
}

func iterateOverCandidates(
	ctx context.Context,
	preemptionCtx *preemptionCtx,
	iterator classical.CandidateIterator,
	allowBorrowing bool,
	yield func(*Target) bool,
) (cont bool) {
	yield = policy.YieldFromSnapshot(preemptionCtx.snapshot, yield)
	iterator.Reset()
	for candidateWl, reason := iterator.Next(allowBorrowing); candidateWl != nil; candidateWl, reason = iterator.Next(allowBorrowing) {
		candidate := &Target{
			WorkloadInfo: candidateWl,
			Reason:       reason,
			WorkloadCq:   preemptionCtx.snapshot.ClusterQueue(candidateWl.ClusterQueue),
		}
		if !yield(candidate) {
			return
		}
	}
	return true
}
