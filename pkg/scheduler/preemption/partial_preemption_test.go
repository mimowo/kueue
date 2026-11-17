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
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	config "sigs.k8s.io/kueue/apis/config/v1beta2"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/scheduler/flavorassigner"
	preemptexpectations "sigs.k8s.io/kueue/pkg/scheduler/preemption/expectations"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
)

func TestPartialPreemptionGetTargets(t *testing.T) {
	standalone := func(quota string) []*kueue.ClusterQueue {
		return []*kueue.ClusterQueue{partialPreemptionClusterQueue(quota)}
	}
	incoming := func(priority int32, cpu string) *kueue.Workload {
		return utiltestingapi.MakeWorkload("in", "").UID("wl-in").Priority(priority).Request(corev1.ResourceCPU, cpu).Obj()
	}
	// The incoming workload goes to the first ClusterQueue, which reclaims its
	// quota from the borrowers of the cohort.
	cohort := func(quotas ...string) []*kueue.ClusterQueue {
		cqs := make([]*kueue.ClusterQueue, len(quotas))
		for i, quota := range quotas {
			cqs[i] = utiltestingapi.MakeClusterQueue(string(rune('a' + i))).Cohort("cohort").
				ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").Resource(corev1.ResourceCPU, quota).Obj()).
				Obj()
		}
		cqs[0].Spec.Preemption = &kueue.ClusterQueuePreemption{ReclaimWithinCohort: kueue.PreemptionPolicyAny}
		return cqs
	}
	// ClusterQueue "b" borrows 4 with two workloads of 4 Pods. b1 has the lower
	// priority, so it is the first candidate, and it cannot shrink. Once b1 is
	// evicted, "b" does not borrow anymore and b2 is not a candidate.
	twoBorrowers := []*kueue.Workload{
		partialPreemptionVictimInCQ("b", "b1", 0, 4, 4),
		partialPreemptionVictimInCQ("b", "b2", 1, 4, 1),
	}
	// "b" and "c" borrow 4 each, and the candidates come in the order b1, b2, c1.
	// Evicting b1 gives back what "b" borrows, so b2 is not a candidate after that
	// even though the shrinking walk, which evicts nobody, sees it. Shrinking c1
	// to its floor of 3 is not enough for a workload that needs 8.
	threeBorrowers := []*kueue.Workload{
		partialPreemptionVictimInCQ("b", "b1", 0, 4, 4),
		partialPreemptionVictimInCQ("b", "b2", 1, 4, 4),
		partialPreemptionVictimInCQ("c", "c1", 2, 4, 3),
	}
	// "a" owns 8, "b" owns 2 and uses 4 with b1 (floor 2), "c" owns 0 and uses 4
	// with c1 (floor 3). The 8 used leave 2 free, so a request of 7 needs 5 more.
	// Shrinking both victims to their floors frees only 2+1=3, so the first
	// candidate, the one with the lower priority, is evicted on top of that.
	floorsAreNotEnough := func(b1Priority, c1Priority int32) []*kueue.Workload {
		return []*kueue.Workload{
			partialPreemptionVictimInCQ("b", "b1", b1Priority, 4, 2),
			partialPreemptionVictimInCQ("c", "c1", c1Priority, 4, 3),
		}
	}
	// "a" owns 8 and may borrow at the expense of workloads with a lower priority,
	// "b" owns 5 and uses 10 with b1 (floor 5). A request of 11 leaves "b" at
	// most 2, which is below the floor of b1: shrinking b1 is not enough.
	borrowingQueues := cohort("8", "5")
	borrowingQueues[0].Spec.Preemption.BorrowWithinCohort = &kueue.BorrowWithinCohort{Policy: kueue.BorrowWithinCohortPolicyLowerPriority}
	belowTheFloor := []*kueue.Workload{partialPreemptionVictimInCQ("b", "b1", 0, 10, 5)}
	// "a" owns 9, "b" owns 3 and uses 11. A request of 11 borrows 2 of them, and
	// fits when b2 is evicted and b1 shrinks to 1 Pod. That is not fair: b1 gives
	// up 4 Pods, and with them "b" borrows 2, not more than the request. Evicting
	// both is not fair either.
	unfairToShrink := []*kueue.Workload{
		partialPreemptionVictimInCQ("b", "b1", 0, 5, 0),
		partialPreemptionVictimInCQ("b", "b2", 1, 6, 2),
	}

	cases := map[string]struct {
		gateDisabled bool
		// finalTargetsNotVerified disables the FairSharingVerifyFinalTargets feature gate.
		finalTargetsNotVerified bool
		fairSharing             *config.FairSharing
		clusterQueues           []*kueue.ClusterQueue
		victims                 []*kueue.Workload
		incoming                *kueue.Workload
		want                    []planTarget
	}{
		"gate on: reduces the victim to free only what the incoming workload needs": {
			clusterQueues: standalone("6"),
			victims:       []*kueue.Workload{partialPreemptionVictim("victim", 0, 5, 1, nil)},
			incoming:      incoming(1, "4"),
			want:          []planTarget{shrink("victim", 2)},
		},
		"gate off: whole eviction": {
			gateDisabled:  true,
			clusterQueues: standalone("6"),
			victims:       []*kueue.Workload{partialPreemptionVictim("victim", 0, 5, 1, nil)},
			incoming:      incoming(1, "4"),
			want:          []planTarget{evict("victim")},
		},
		"no fit: do not return an in-flight partial target": {
			clusterQueues: standalone("6"),
			victims:       []*kueue.Workload{partialPreemptionVictim("victim", 0, 5, 1, new(int32(2)))},
			incoming:      incoming(1, "7"),
		},
		"reclaims only enough": {
			clusterQueues: standalone("20"),
			victims:       []*kueue.Workload{partialPreemptionVictim("victim", 0, 20, 5, nil)},
			incoming:      incoming(1, "10"),
			want:          []planTarget{shrink("victim", 10)},
		},
		"multiple candidates each partially reduced": {
			clusterQueues: standalone("10"),
			victims: []*kueue.Workload{
				partialPreemptionVictim("v1", 0, 5, 4, nil),
				partialPreemptionVictim("v2", 0, 5, 4, nil),
			},
			incoming: incoming(1, "2"),
			want:     []planTarget{shrink("v1", 4), shrink("v2", 4)},
		},
		"a whole-only candidate is evicted, and the targets keep the order of the priorities": {
			clusterQueues: standalone("15"),
			victims: []*kueue.Workload{
				partialPreemptionVictim("H1", 30, 5, 1, nil),
				partialPreemptionVictim("M1", 20, 5, 1, nil),
				partialPreemptionVictim("L1", 10, 5, 5, nil),
			},
			incoming: incoming(40, "12"),
			want:     []planTarget{evict("L1"), shrink("M1", 1), shrink("H1", 2)},
		},
		"shrinking is not enough: progresses to full eviction": {
			clusterQueues: standalone("10"),
			victims: []*kueue.Workload{
				partialPreemptionVictim("victim-a", 0, 5, 1, nil),
				partialPreemptionVictim("victim-b", 0, 5, 5, nil),
			},
			incoming: incoming(1, "8"),
			want:     []planTarget{shrink("victim-a", 2), evict("victim-b")},
		},
		"evicted partial-preemption victim is wholly evicted": {
			clusterQueues: standalone("6"),
			victims: []*kueue.Workload{func() *kueue.Workload {
				wl := partialPreemptionVictim("victim", 0, 5, 1, nil)
				workloadevict.SetEvictedCondition(wl, time.Now(), kueue.WorkloadEvictedByPreemption, "already evicted")
				return wl
			}()},
			incoming: incoming(1, "4"),
			want:     []planTarget{evict("victim")},
		},
		"TAS victim is wholly evicted": {
			clusterQueues: standalone("6"),
			victims: []*kueue.Workload{func() *kueue.Workload {
				wl := partialPreemptionVictim("victim", 0, 5, 1, nil)
				wl.Spec.PodSets[0].TopologyRequest = &kueue.PodSetTopologyRequest{Required: new("rack")}
				wl.Status.Admission.PodSetAssignments[0].TopologyAssignment = utiltestingapi.MakeTopologyAssignment([]string{"rack"}).Obj()
				return wl
			}()},
			incoming: incoming(1, "4"),
			want:     []planTarget{evict("victim")},
		},
		"multiple shrinkable PodSets are wholly evicted": {
			clusterQueues: standalone("12"),
			victims: []*kueue.Workload{func() *kueue.Workload {
				wl := partialPreemptionVictim("victim", 0, 5, 1, nil)
				second := utiltestingapi.MakePodSet("second", 5).Request(corev1.ResourceCPU, "1").SetPreemptionMinimumCount(1).Obj()
				wl.Spec.PodSets = append(wl.Spec.PodSets, *second)
				assignment := utiltestingapi.MakePodSetAssignment("second").Assignment(corev1.ResourceCPU, "default", "5").Count(5).Obj()
				wl.Status.Admission.PodSetAssignments = append(wl.Status.Admission.PodSetAssignments, assignment)
				return wl
			}()},
			incoming: incoming(1, "4"),
			want:     []planTarget{evict("victim")},
		},
		"cohort, gate off: the first borrower is evicted": {
			gateDisabled:  true,
			clusterQueues: cohort("4", "4"),
			victims:       twoBorrowers,
			incoming:      incoming(0, "3"),
			want:          []planTarget{evict("b1")},
		},
		"cohort: a borrower that the eviction of the first one would spare shrinks instead": {
			clusterQueues: cohort("4", "4"),
			victims:       twoBorrowers,
			incoming:      incoming(0, "3"),
			want:          []planTarget{shrink("b2", 1)},
		},
		"cohort, gate off: a borrower is not evicted once its ClusterQueue does not borrow anymore": {
			gateDisabled:  true,
			clusterQueues: cohort("8", "4", "0"),
			victims:       threeBorrowers,
			incoming:      incoming(0, "8"),
			want:          []planTarget{evict("b1"), evict("c1")},
		},
		"cohort: a borrower is not evicted once its ClusterQueue does not borrow anymore": {
			clusterQueues: cohort("8", "4", "0"),
			victims:       threeBorrowers,
			incoming:      incoming(0, "8"),
			want:          []planTarget{evict("b1"), evict("c1")},
		},
		// Evicting c1 with b1 at its floor frees 4+2=6, one more than needed.
		"cohort: the first candidate is evicted, and the other one grows back from its floor": {
			clusterQueues: cohort("8", "2", "0"),
			victims:       floorsAreNotEnough(1, 0),
			incoming:      incoming(0, "7"),
			want:          []planTarget{evict("c1"), shrink("b1", 3)},
		},
		// Evicting b1 with c1 at its floor frees 4+1=5, exactly what is needed.
		"cohort: the first candidate is evicted, and the other one stays at its floor": {
			clusterQueues: cohort("8", "2", "0"),
			victims:       floorsAreNotEnough(0, 1),
			incoming:      incoming(0, "7"),
			want:          []planTarget{evict("b1"), shrink("c1", 3)},
		},
		"cohort, gate off: a victim that would have to shrink below its floor is evicted": {
			gateDisabled:  true,
			clusterQueues: borrowingQueues,
			victims:       belowTheFloor,
			incoming:      incoming(10, "11"),
			want:          []planTarget{evict("b1")},
		},
		"cohort: a victim that would have to shrink below its floor is evicted": {
			clusterQueues: borrowingQueues,
			victims:       belowTheFloor,
			incoming:      incoming(10, "11"),
			want:          []planTarget{evict("b1")},
		},
		"Fair Sharing, gate off: the borrower is evicted": {
			gateDisabled:  true,
			fairSharing:   &config.FairSharing{},
			clusterQueues: cohort("4", "4"),
			victims:       []*kueue.Workload{partialPreemptionVictimInCQ("b", "b1", 0, 8, 2)},
			incoming:      incoming(0, "4"),
			want:          []planTarget{evict("b1")},
		},
		"Fair Sharing: a borrower that cannot shrink is evicted": {
			fairSharing:   &config.FairSharing{},
			clusterQueues: cohort("4", "4"),
			victims:       []*kueue.Workload{partialPreemptionVictimInCQ("b", "b1", 0, 8, 8)},
			incoming:      incoming(0, "4"),
			want:          []planTarget{evict("b1")},
		},
		"Fair Sharing: the borrower shrinks to what its ClusterQueue owns": {
			fairSharing:   &config.FairSharing{},
			clusterQueues: cohort("4", "4"),
			victims:       []*kueue.Workload{partialPreemptionVictimInCQ("b", "b1", 0, 8, 2)},
			incoming:      incoming(0, "4"),
			want:          []planTarget{shrink("b1", 4)},
		},
		// The final targets are verified like the targets without partial
		// preemption: only with the FairSharingVerifyFinalTargets feature gate.
		"Fair Sharing: a shrink is verified with the usage that the victim gives up": {
			fairSharing:   &config.FairSharing{},
			clusterQueues: cohort("9", "3"),
			victims:       unfairToShrink,
			incoming:      incoming(0, "11"),
		},
		"Fair Sharing, FairSharingVerifyFinalTargets off: a shrink is not verified": {
			finalTargetsNotVerified: true,
			fairSharing:             &config.FairSharing{},
			clusterQueues:           cohort("9", "3"),
			victims:                 unfairToShrink,
			incoming:                incoming(0, "11"),
			want:                    []planTarget{shrink("b1", 1), evict("b2")},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.PartialPreemption, !tc.gateDisabled)
			features.SetFeatureGateDuringTest(t, features.FairSharingVerifyFinalTargets, !tc.finalTargetsNotVerified)
			fixture := newPlanFixture(t, tc.fairSharing, tc.clusterQueues, tc.victims, tc.incoming)

			got := fixture.targets(t, fixture.preemptor.getPreemptionStrategyIterator(fixture.ctx, fixture.pCtx))

			if diff := cmp.Diff(tc.want, got, planTargetCmpOpts); diff != "" {
				t.Errorf("Unexpected targets (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPartialPreemptionIssuePreemptions(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.PartialPreemption, true)

	type victimWant struct {
		partial     bool
		targetCount int32
	}
	cases := map[string]struct {
		quota         string
		victims       []*kueue.Workload
		incoming      *kueue.Workload
		wantPreempted int
		wantFailed    int
		want          map[string]victimWant
	}{
		"partial scale-down keeps the victim running": {
			quota:         "6",
			victims:       []*kueue.Workload{partialPreemptionVictim("victim", 0, 5, 1, nil)},
			incoming:      utiltestingapi.MakeWorkload("in", "").UID("wl-in").Priority(1).Request(corev1.ResourceCPU, "4").Obj(),
			wantPreempted: 1,
			want:          map[string]victimWant{"victim": {partial: true, targetCount: 2}},
		},
		"non-shrinkable victim is wholly evicted": {
			// minCount equals the admitted count, so the victim cannot scale down and is evicted.
			quota:         "6",
			victims:       []*kueue.Workload{partialPreemptionVictim("victim", 0, 5, 5, nil)},
			incoming:      utiltestingapi.MakeWorkload("in", "").UID("wl-in").Priority(1).Request(corev1.ResourceCPU, "4").Obj(),
			wantPreempted: 1,
			want:          map[string]victimWant{"victim": {partial: false}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			items := make([]kueue.Workload, len(tc.victims))
			for i := range tc.victims {
				items[i] = *tc.victims[i]
			}
			ctx, log := utiltesting.ContextWithLog(t)
			cl := utiltesting.NewClientBuilder().
				WithLists(&kueue.WorkloadList{Items: items}).
				WithStatusSubresource(&kueue.Workload{}).
				WithInterceptorFuncs(interceptor.Funcs{SubResourceApply: utiltesting.TreatSSAAsStrategicMergeForApplyConfiguration}).
				Build()
			cqCache := schdcache.New(cl)
			cqCache.AddOrUpdateResourceFlavor(log, utiltestingapi.MakeResourceFlavor("default").Obj())
			if err := cqCache.AddClusterQueue(ctx, partialPreemptionClusterQueue(tc.quota)); err != nil {
				t.Fatalf("Couldn't add ClusterQueue to cache: %v", err)
			}
			recorder := &utiltesting.EventRecorder{}
			preemptor := New(cl, workload.Ordering{}, recorder, nil, false, clocktesting.NewFakeClock(now), nil, preemptexpectations.New(), nil)
			snapshot, err := cqCache.Snapshot(ctx)
			if err != nil {
				t.Fatalf("unexpected error while building snapshot: %v", err)
			}
			wlInfo := workload.NewInfo(log, tc.incoming)
			wlInfo.ClusterQueue = "standalone"
			assignment := singlePodSetAssignment(flavorassigner.ResourceAssignment{
				corev1.ResourceCPU: &flavorassigner.FlavorAssignment{Name: "default", Mode: flavorassigner.Preempt},
			})
			strategies := preemptor.GetPreemptionStrategyIterator(ctx, *wlInfo, snapshot, assignment)
			targets := preemptor.GetTargetsWithStrategy(ctx, strategies)

			preempted, failed, err := preemptor.IssuePreemptions(ctx, cqCache, wlInfo, targets, snapshot.ClusterQueue(wlInfo.ClusterQueue))
			if err != nil {
				t.Fatalf("IssuePreemptions failed: %v", err)
			}
			if preempted != tc.wantPreempted || failed != tc.wantFailed {
				t.Errorf("IssuePreemptions = (preempted=%d, failed=%d), want (%d, %d)", preempted, failed, tc.wantPreempted, tc.wantFailed)
			}

			for victimName, want := range tc.want {
				got := &kueue.Workload{}
				if err := cl.Get(ctx, types.NamespacedName{Name: victimName}, got); err != nil {
					t.Fatalf("failed to get victim %q: %v", victimName, err)
				}
				if !want.partial {
					if !workloadevict.IsEvicted(got) {
						t.Errorf("victim %q was not evicted, want whole eviction", victimName)
					}
					continue
				}
				// The scale-down request is persisted on the executor assignment...
				gotCount, ok := admittedTargetCount(got, "executor")
				if !ok || gotCount != want.targetCount {
					t.Errorf("victim %q executor targetCount = (%d, %t), want (%d, true)", victimName, gotCount, ok, want.targetCount)
				}
				// ...and the quota-accounted count is NOT changed here (released only as pods terminate)...
				if c := admittedCount(got, "executor"); c != 5 {
					t.Errorf("victim %q executor admitted count = %d, want 5 (unchanged; released as pods exit)", victimName, c)
				}
				// ...and the victim is NOT evicted (keeps running at reduced size).
				if workloadevict.IsEvicted(got) {
					t.Errorf("victim %q was evicted, want it running (partial preemption must not evict)", victimName)
				}
			}
		})
	}
}

func TestSimulateRemovalOfTargets(t *testing.T) {
	cpu := resources.FlavorResource{Flavor: "default", Resource: corev1.ResourceCPU}
	cases := map[string]struct {
		// retained makes the target a shrink to that many Pods.
		retained *int32
		// released tells that the usage of the victim is already released.
		released  bool
		wantUsage int64
	}{
		"an evicted target gives up all of its usage": {
			wantUsage: 0,
		},
		"a shrunk target gives up the usage of the Pods that it does not retain": {
			retained:  new(int32(2)),
			wantUsage: 2_000,
		},
		"a shrunk target whose usage is already released gives up nothing": {
			retained:  new(int32(2)),
			released:  true,
			wantUsage: 0,
		},
	}
	for name, tc := range cases {
		for _, withPods := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, withPods=%t", name, withPods), func(t *testing.T) {
				victim := partialPreemptionVictim("victim", 0, 5, 1, nil)
				fixture := newPlanFixture(t, nil, []*kueue.ClusterQueue{partialPreemptionClusterQueue("6")}, []*kueue.Workload{victim},
					utiltestingapi.MakeWorkload("in", "").Request(corev1.ResourceCPU, "4").Obj())
				snapshot, cq := fixture.pCtx.snapshot, fixture.pCtx.preemptorCQ
				key := workload.Key(victim)
				target := &Target{WorkloadInfo: cq.Workloads[key]}
				if tc.retained != nil {
					target.PartialPreemption = &policy.PartialPreemptionTarget{
						PodSet:        "executor",
						RetainedCount: *tc.retained,
						ReducedInfo:   reducedInfoForPartialPreemption(target.WorkloadInfo, "executor", *tc.retained),
					}
				}
				if tc.released {
					snapshot.ReleaseWorkloadUsage(target.WorkloadInfo)
				}
				usageBefore := cq.ResourceNode.Usage[cpu]

				// Only the removal with the Pods takes an evicted target out of its ClusterQueue.
				var restore func()
				wantInQueue := true
				if withPods {
					_, log := utiltesting.ContextWithLog(t)
					restore = SimulateRemovals(fixture.ctx, log, snapshot, []*Target{target})
					wantInQueue = tc.retained != nil
				} else {
					restore = SimulateUsageRemoval(snapshot, []*Target{target})
				}

				if got := cq.ResourceNode.Usage[cpu]; got.CmpInt64(tc.wantUsage) != 0 {
					t.Errorf("Usage after the removal is %v, want %d", got, tc.wantUsage)
				}
				if got := cq.Workloads[key] == target.WorkloadInfo; got != wantInQueue {
					t.Errorf("The victim is in its ClusterQueue after the removal: %t, want %t", got, wantInQueue)
				}
				restore()
				if got := cq.ResourceNode.Usage[cpu]; got.Cmp(usageBefore) != 0 {
					t.Errorf("Usage after the restore is %v, want %v", got, usageBefore)
				}
				if cq.Workloads[key] != target.WorkloadInfo {
					t.Errorf("The victim is not in its ClusterQueue after the restore")
				}
			})
		}
	}
}

func admittedTargetCount(wl *kueue.Workload, podSet kueue.PodSetReference) (int32, bool) {
	if wl.Status.Admission == nil {
		return 0, false
	}
	for i := range wl.Status.Admission.PodSetAssignments {
		psa := &wl.Status.Admission.PodSetAssignments[i]
		if psa.Name == podSet && psa.PartialPreemption.RetainedCount != nil {
			return *psa.PartialPreemption.RetainedCount, true
		}
	}
	return 0, false
}

func admittedCount(wl *kueue.Workload, podSet kueue.PodSetReference) int32 {
	if wl.Status.Admission == nil {
		return 0
	}
	for i := range wl.Status.Admission.PodSetAssignments {
		psa := &wl.Status.Admission.PodSetAssignments[i]
		if psa.Name == podSet {
			return ptr.Deref(psa.Count, 0)
		}
	}
	return 0
}

func partialPreemptionClusterQueue(cpuQuota string) *kueue.ClusterQueue {
	return utiltestingapi.MakeClusterQueue("standalone").
		ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
			Resource(corev1.ResourceCPU, cpuQuota).Obj()).
		Preemption(kueue.ClusterQueuePreemption{
			WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
		}).
		Obj()
}

// partialPreemptionVictim builds an elastic victim admitted at usedCount replicas with
// the given minimum count. When preemptionRetainedCount is non-nil it seeds the executor
// assignment with an in-flight scale-down request (as a prior preemption round would).
func partialPreemptionVictim(name string, priorityValue, usedCount, minCount int32, preemptionRetainedCount *int32) *kueue.Workload {
	wl := partialPreemptionVictimInCQ("standalone", name, priorityValue, usedCount, minCount)
	if preemptionRetainedCount != nil {
		wl.Status.Admission.PodSetAssignments[0].PartialPreemption = &kueue.PodSetPartialPreemptionStatus{RetainedCount: preemptionRetainedCount}
	}
	return wl
}

// partialPreemptionAdmittedAt is when every victim got its quota, so that
// candidates of the same priority come in the order of their names.
var partialPreemptionAdmittedAt = time.Now().Truncate(time.Second)

// partialPreemptionVictimInCQ is like partialPreemptionVictim, but admits the victim
// into cq instead of the fixed "standalone" ClusterQueue, for scenarios that need more
// than one ClusterQueue (e.g. a cohort with borrowing).
func partialPreemptionVictimInCQ(cq kueue.ClusterQueueReference, name string, priorityValue, usedCount, minCount int32) *kueue.Workload {
	return utiltestingapi.MakeWorkload(name, "").
		UID(types.UID(name)).
		Annotation(constants.PartialPreemptionAnnotation, "true").
		Priority(priorityValue).
		PodSets(*utiltestingapi.MakePodSet("executor", int(usedCount)).
			Request(corev1.ResourceCPU, "1").
			SetPreemptionMinimumCount(minCount).
			Obj()).
		ReserveQuotaAt(utiltestingapi.MakeAdmission(cq).
			PodSets(utiltestingapi.MakePodSetAssignment("executor").
				Assignment(corev1.ResourceCPU, "default", fmt.Sprintf("%d", usedCount)).
				Count(usedCount).
				Obj()).
			Obj(), partialPreemptionAdmittedAt).
		Obj()
}
