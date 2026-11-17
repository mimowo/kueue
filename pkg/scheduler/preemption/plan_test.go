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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	clocktesting "k8s.io/utils/clock/testing"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/scheduler/flavorassigner"
	preemptexpectations "sigs.k8s.io/kueue/pkg/scheduler/preemption/expectations"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
)

// victimPodSet is the shrinkable PodSet used by partialPreemptionVictim.
const victimPodSet kueue.PodSetReference = "executor"

// byName orders victims by name, standing in for the preemption-appeal ordering in
// tests that only exercise the merge and backfill mechanics.
func byName(a, b *workload.Info) int {
	return strings.Compare(a.Obj.Name, b.Obj.Name)
}

// newPlanPreemptionCtx builds a preemptionCtx for a single-CQ setup the way
// getPartialPreemptionTargets does, but without running the strategies. The incoming
// workload requests incomingCPU of CPU in the fixture's CQ.
func newPlanPreemptionCtx(t *testing.T, quota string, victims []*kueue.Workload, incomingCPU string) (context.Context, *preemptionCtx) {
	t.Helper()
	ctx, log := utiltesting.ContextWithLog(t)
	items := make([]kueue.Workload, len(victims))
	for i := range victims {
		items[i] = *victims[i]
	}
	cl := utiltesting.NewClientBuilder().
		WithLists(&kueue.WorkloadList{Items: items}).
		Build()
	cqCache := schdcache.New(cl)
	cqCache.AddOrUpdateResourceFlavor(log, utiltestingapi.MakeResourceFlavor("default").Obj())
	cq := partialPreemptionClusterQueue(quota)
	if err := cqCache.AddClusterQueue(ctx, cq); err != nil {
		t.Fatalf("Couldn't add ClusterQueue to cache: %v", err)
	}
	preemptor := New(cl, workload.Ordering{}, &utiltesting.EventRecorder{}, nil, false,
		clocktesting.NewFakeClock(time.Now()), nil, preemptexpectations.New(), nil)
	snapshot, err := cqCache.Snapshot(ctx)
	if err != nil {
		t.Fatalf("unexpected error while building snapshot: %v", err)
	}
	wlInfo := workload.NewInfo(log, utiltestingapi.MakeWorkload("in", "").
		Request(corev1.ResourceCPU, incomingCPU).Obj())
	wlInfo.ClusterQueue = kueue.ClusterQueueReference(cq.Name)
	pCtx := preemptor.buildContext(ctx, *wlInfo, singlePodSetAssignment(flavorassigner.ResourceAssignment{
		corev1.ResourceCPU: &flavorassigner.FlavorAssignment{Name: "default", Mode: flavorassigner.Preempt},
	}), snapshot)
	return ctx, pCtx
}

// snapshotExecutorCount returns the victim's executor count in the snapshot, or false
// when the victim is absent from it.
func snapshotExecutorCount(pCtx *preemptionCtx, wl *kueue.Workload) (int32, bool) {
	info := pCtx.preemptorCQ.Workloads[workload.Key(wl)]
	if info == nil {
		return 0, false
	}
	return podSetRequestCount(info, victimPodSet), true
}

func TestPlanPreemption(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.PartialPreemption, true)
	_, log := utiltesting.ContextWithLog(t)

	nonElastic := utiltestingapi.MakeWorkload("c", "").
		PodSets(*utiltestingapi.MakePodSet(victimPodSet, 5).
			Request(corev1.ResourceCPU, "1").SetPreemptionMinimumCount(1).Obj()).
		Obj()
	evicted := partialPreemptionVictim("evicted", 0, 5, 1, nil)
	workloadevict.SetEvictedCondition(evicted, time.Now(), kueue.WorkloadEvictedByPreemption, "test")

	cases := map[string]struct {
		candidate *kueue.Workload
		// currentAtFloor shrinks the candidate's live snapshot instance to its floor,
		// as an earlier walk pass would.
		currentAtFloor bool
		wantTarget     bool
	}{
		"non-elastic candidate is not shrinkable": {
			candidate: nonElastic,
		},
		"evicted candidate is not shrinkable": {
			candidate: evicted,
		},
		"candidate at its floor is not shrinkable": {
			candidate: partialPreemptionVictim("at-floor", 0, 5, 5, nil),
		},
		"live instance already at the floor is not shrunk again": {
			candidate:      partialPreemptionVictim("shrunk", 0, 5, 1, nil),
			currentAtFloor: true,
		},
		"shrinks the candidate to its floor": {
			candidate:  partialPreemptionVictim("shrinkable", 0, 5, 1, nil),
			wantTarget: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := workload.NewInfo(log, tc.candidate)
			current := candidate
			if tc.currentAtFloor {
				floor, _ := workload.PreemptionMinRetainedCount(tc.candidate, victimPodSet)
				current = reducedInfoForPartialPreemption(candidate, victimPodSet, floor)
			}

			target := planPreemption(&Target{WorkloadInfo: candidate, Reason: "test"}, current)
			if !tc.wantTarget {
				if target != nil {
					t.Fatalf("planPreemption() = %+v, want nil", target)
				}
				return
			}
			if target == nil {
				t.Fatal("planPreemption() = nil, want a partial target")
			}
			if target.WorkloadInfo != candidate {
				t.Errorf("target.WorkloadInfo is not the original candidate")
			}
			if target.Reason != "test" {
				t.Errorf("target.Reason = %q, want %q", target.Reason, "test")
			}
			if target.PartialPreemption == nil {
				t.Fatal("target.PartialPreemption = nil, want a shrink to the floor")
			}
			wantFloor, _ := workload.PreemptionMinRetainedCount(tc.candidate, victimPodSet)
			if got := target.PartialPreemption.PodSet; got != victimPodSet {
				t.Errorf("PartialPreemption.PodSet = %q, want %q", got, victimPodSet)
			}
			if got := target.PartialPreemption.RetainedCount; got != wantFloor {
				t.Errorf("PartialPreemption.RetainedCount = %d, want %d", got, wantFloor)
			}
			if got := podSetRequestCount(target.PartialPreemption.ReducedInfo, victimPodSet); got != wantFloor {
				t.Errorf("ReducedInfo executor count = %d, want %d", got, wantFloor)
			}
		})
	}
}

func TestFillbackPartialPreemption(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.PartialPreemption, true)
	const victimName = "victim"

	cases := map[string]struct {
		quota       string
		incomingCPU string
		victim      *kueue.Workload
		// wholeEvicted marks how walk left the victim: escalated to a whole eviction
		// (absent from the snapshot) or only shrunk to its floor (present at the floor).
		wholeEvicted bool
		wantDropped  bool
		// wantRetained is the resulting target's retained count; nil means the target
		// became a whole eviction (ignored when wantDropped).
		wantRetained *int32
		// wantSnapshot is the victim's executor count left in the snapshot; nil means
		// the victim is absent from it.
		wantSnapshot *int32
	}{
		"fully restores and drops the target when the preemptor fits": {
			quota: "6", incomingCPU: "1",
			victim:       partialPreemptionVictim(victimName, 0, 5, 1, nil),
			wantDropped:  true,
			wantSnapshot: new(int32(5)),
		},
		"grows back to the largest count that fits": {
			quota: "6", incomingCPU: "4",
			victim:       partialPreemptionVictim(victimName, 0, 5, 1, nil),
			wantRetained: new(int32(2)),
			wantSnapshot: new(int32(2)),
		},
		"never grows above the in-flight retained count": {
			quota: "6", incomingCPU: "4",
			victim:       partialPreemptionVictim(victimName, 0, 5, 1, new(int32(1))),
			wantRetained: new(int32(1)),
			wantSnapshot: new(int32(1)),
		},
		"settles at the floor when only the floor fits": {
			quota: "6", incomingCPU: "5",
			victim:       partialPreemptionVictim(victimName, 0, 5, 1, nil),
			wantRetained: new(int32(1)),
			wantSnapshot: new(int32(1)),
		},
		"whole-evicted victim stays evicted when even the floor does not fit": {
			quota: "6", incomingCPU: "6",
			victim:       partialPreemptionVictim(victimName, 0, 5, 1, nil),
			wholeEvicted: true,
			wantRetained: nil,
			wantSnapshot: nil,
		},
		"shrunk victim settles at the floor when even the floor does not fit": {
			quota: "6", incomingCPU: "6",
			victim:       partialPreemptionVictim(victimName, 0, 5, 1, nil),
			wantRetained: new(int32(1)),
			wantSnapshot: new(int32(1)),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, pCtx := newPlanPreemptionCtx(t, tc.quota, []*kueue.Workload{tc.victim}, tc.incomingCPU)
			info := pCtx.preemptorCQ.Workloads[workload.Key(tc.victim)]
			if info == nil {
				t.Fatal("victim missing from the snapshot")
			}
			target := &Target{WorkloadInfo: info, WorkloadCq: pCtx.preemptorCQ}

			// Pin the state walk left behind before the search mutates the snapshot, and
			// stage the target the way walk does: a shrink to the floor when the victim
			// was only shrunk, a bare target when it was escalated to a whole eviction.
			floor, _ := workload.PreemptionMinRetainedCount(tc.victim, victimPodSet)
			pCtx.snapshot.RemoveWorkload(info)
			if !tc.wholeEvicted {
				pCtx.snapshot.AddWorkload(reducedInfoForPartialPreemption(info, victimPodSet, floor))
				target.PartialPreemption = &policy.PartialPreemptionTarget{
					PodSet:        victimPodSet,
					RetainedCount: floor,
					ReducedInfo:   reducedInfoForPartialPreemption(info, victimPodSet, floor),
				}
			}

			dropped := fillbackPartialPreemption(ctx, pCtx, target, victimPodSet, false)
			if dropped != tc.wantDropped {
				t.Errorf("fillbackPartialPreemption() dropped = %t, want %t", dropped, tc.wantDropped)
			}

			gotCount, present := snapshotExecutorCount(pCtx, tc.victim)
			switch {
			case tc.wantSnapshot == nil:
				if present {
					t.Errorf("victim present in the snapshot with count %d, want absent", gotCount)
				}
			case !present:
				t.Errorf("victim absent from the snapshot, want count %d", *tc.wantSnapshot)
			case gotCount != *tc.wantSnapshot:
				t.Errorf("victim snapshot count = %d, want %d", gotCount, *tc.wantSnapshot)
			}

			if tc.wantDropped {
				return
			}
			if tc.wantRetained == nil {
				if target.PartialPreemption != nil {
					t.Errorf("target.PartialPreemption = %+v, want a whole eviction (nil)", target.PartialPreemption)
				}
				return
			}
			if target.PartialPreemption == nil {
				t.Fatal("target.PartialPreemption = nil, want a shrink")
			}
			if got := target.PartialPreemption.RetainedCount; got != *tc.wantRetained {
				t.Errorf("PartialPreemption.RetainedCount = %d, want %d", got, *tc.wantRetained)
			}
			if got := podSetRequestCount(target.PartialPreemption.ReducedInfo, victimPodSet); got != *tc.wantRetained {
				t.Errorf("ReducedInfo executor count = %d, want %d", got, *tc.wantRetained)
			}
		})
	}
}

func TestMergeStagedTargets(t *testing.T) {
	_, log := utiltesting.ContextWithLog(t)
	mk := func(name string) *Target {
		return &Target{WorkloadInfo: workload.NewInfo(log, utiltestingapi.MakeWorkload(name, "").Obj())}
	}
	partial := mk("v")
	partial.PartialPreemption = &policy.PartialPreemptionTarget{PodSet: victimPodSet, RetainedCount: 1}
	whole := mk("v")
	w := mk("w")

	pl := &partialPlan{ordering: byName}
	cases := map[string]struct {
		targets []*Target
		want    []*Target
	}{
		"keeps the partial stage when it is yielded first": {
			targets: []*Target{partial, whole, w},
			want:    []*Target{partial, w},
		},
		"keeps the first stage regardless of the yield order": {
			targets: []*Target{w, whole, partial},
			want:    []*Target{whole, w},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := pl.mergeStagedTargets(tc.targets)
			if len(got) != len(tc.want) {
				t.Fatalf("mergeStagedTargets() returned %d targets, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("merged[%d] = %q, want the %q stage",
						i, got[i].WorkloadInfo.Obj.Name, tc.want[i].WorkloadInfo.Obj.Name)
				}
			}
		})
	}
}

func TestPartialPlanFillBack(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.PartialPreemption, true)

	v := partialPreemptionVictim("v", 0, 5, 1, nil)
	n := partialPreemptionVictim("n", 0, 5, 5, nil)

	cases := map[string]struct {
		quota        string
		incomingCPU  string
		victims      []*kueue.Workload
		makeTargets  func(origByName map[string]*workload.Info, cq *schdcache.ClusterQueueSnapshot) []*Target
		wantRetained map[string]*int32
		wantSnapshot map[string]*int32
	}{
		"merges a victim's stages and backfills it over the partial range": {
			quota: "9", incomingCPU: "5",
			victims: []*kueue.Workload{v, n},
			makeTargets: func(origByName map[string]*workload.Info, cq *schdcache.ClusterQueueSnapshot) []*Target {
				vInfo := origByName["v"]
				floor, _ := workload.PreemptionMinRetainedCount(v, victimPodSet)
				return []*Target{
					{
						WorkloadInfo: vInfo, Reason: "shrink", WorkloadCq: cq,
						PartialPreemption: &policy.PartialPreemptionTarget{
							PodSet: victimPodSet, RetainedCount: floor,
							ReducedInfo: reducedInfoForPartialPreemption(vInfo, victimPodSet, floor),
						},
					},
					{WorkloadInfo: vInfo, Reason: "whole", WorkloadCq: cq},
					{WorkloadInfo: origByName["n"], Reason: "whole", WorkloadCq: cq},
				}
			},
			wantRetained: map[string]*int32{"v": new(int32(4)), "n": nil},
			wantSnapshot: map[string]*int32{"v": new(int32(4)), "n": nil},
		},
		"drops every target when the preemptor no longer needs the quota": {
			quota: "20", incomingCPU: "5",
			victims: []*kueue.Workload{v, n},
			makeTargets: func(origByName map[string]*workload.Info, cq *schdcache.ClusterQueueSnapshot) []*Target {
				vInfo := origByName["v"]
				floor, _ := workload.PreemptionMinRetainedCount(v, victimPodSet)
				return []*Target{
					{
						WorkloadInfo: vInfo, Reason: "shrink", WorkloadCq: cq,
						PartialPreemption: &policy.PartialPreemptionTarget{
							PodSet: victimPodSet, RetainedCount: floor,
							ReducedInfo: reducedInfoForPartialPreemption(vInfo, victimPodSet, floor),
						},
					},
					{WorkloadInfo: vInfo, Reason: "whole", WorkloadCq: cq},
					{WorkloadInfo: origByName["n"], Reason: "whole", WorkloadCq: cq},
				}
			},
			wantRetained: map[string]*int32{},
			wantSnapshot: map[string]*int32{"v": new(int32(5)), "n": new(int32(5))},
		},
		"routes a whole-eviction stage of a shrinkable victim through the partial search": {
			quota: "9", incomingCPU: "5",
			victims: []*kueue.Workload{v},
			makeTargets: func(origByName map[string]*workload.Info, cq *schdcache.ClusterQueueSnapshot) []*Target {
				return []*Target{
					{WorkloadInfo: origByName["v"], Reason: "whole", WorkloadCq: cq},
				}
			},
			wantRetained: map[string]*int32{"v": new(int32(4))},
			wantSnapshot: map[string]*int32{"v": new(int32(4))},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, pCtx := newPlanPreemptionCtx(t, tc.quota, tc.victims, tc.incomingCPU)
			cq := pCtx.preemptorCQ

			origByName := make(map[string]*workload.Info, len(tc.victims))
			for _, wl := range tc.victims {
				info := cq.Workloads[workload.Key(wl)]
				if info == nil {
					t.Fatalf("workload %q missing from the snapshot", wl.Name)
				}
				origByName[wl.Name] = info
			}
			// Walk ended with a whole eviction of every victim.
			for _, info := range origByName {
				pCtx.snapshot.RemoveWorkload(info)
			}

			targets := tc.makeTargets(origByName, cq)

			pl := &partialPlan{pCtx: pCtx, ordering: byName}
			got := pl.fillBack(ctx, targets)

			if len(got) != len(tc.wantRetained) {
				t.Fatalf("fillBack() returned %d targets, want %d: %+v", len(got), len(tc.wantRetained), got)
			}
			gotByName := make(map[string]*Target, len(got))
			for _, target := range got {
				gotByName[target.WorkloadInfo.Obj.Name] = target
			}
			for name, wantRetained := range tc.wantRetained {
				target, ok := gotByName[name]
				if !ok {
					t.Errorf("victim %q missing from the fillBack result", name)
					continue
				}
				if wantRetained == nil {
					if target.PartialPreemption != nil {
						t.Errorf("victim %q: PartialPreemption = %+v, want a whole eviction (nil)", name, target.PartialPreemption)
					}
					continue
				}
				if target.PartialPreemption == nil {
					t.Errorf("victim %q: PartialPreemption = nil, want a shrink to %d", name, *wantRetained)
					continue
				}
				if got := target.PartialPreemption.RetainedCount; got != *wantRetained {
					t.Errorf("victim %q: RetainedCount = %d, want %d", name, got, *wantRetained)
				}
			}
			for name, wantCount := range tc.wantSnapshot {
				gotCount, present := snapshotExecutorCount(pCtx, origByName[name].Obj)
				if wantCount == nil {
					if present {
						t.Errorf("victim %q present in the snapshot with count %d, want absent", name, gotCount)
					}
					continue
				}
				if !present {
					t.Errorf("victim %q absent from the snapshot, want count %d", name, *wantCount)
				} else if gotCount != *wantCount {
					t.Errorf("victim %q snapshot count = %d, want %d", name, gotCount, *wantCount)
				}
			}
		})
	}
}

// scriptedIterator replays a fixed candidate sequence; walk resets it between passes.
type scriptedIterator struct {
	candidates []*workload.Info
	reason     string
	idx        int
}

var _ candidateIterator = (*scriptedIterator)(nil)

func (s *scriptedIterator) Reset() { s.idx = 0 }

func (s *scriptedIterator) Close() {}

func (s *scriptedIterator) Next(borrow bool) *Target {
	if s.idx >= len(s.candidates) {
		return nil
	}
	wl := s.candidates[s.idx]
	s.idx++
	return &Target{WorkloadInfo: wl, Reason: s.reason}
}

// wantStage is a walk outcome: a partial target retaining the given count, or a whole
// eviction when retained is nil.
type wantStage struct {
	name     string
	retained *int32
}

// assertStages checks the yielded targets against the expected stage sequence.
func assertStages(t *testing.T, got []*Target, want []wantStage) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("walk yielded %d targets, want %d", len(got), len(want))
	}
	for i := range want {
		target := got[i]
		if gotName := target.WorkloadInfo.Obj.Name; gotName != want[i].name {
			t.Fatalf("target[%d] = %q, want %q", i, gotName, want[i].name)
		}
		if want[i].retained == nil {
			if target.PartialPreemption != nil {
				t.Errorf("target[%d] (%q): PartialPreemption = %+v, want a whole eviction (nil)",
					i, want[i].name, target.PartialPreemption)
			}
			continue
		}
		if target.PartialPreemption == nil {
			t.Errorf("target[%d] (%q): PartialPreemption = nil, want a shrink to %d",
				i, want[i].name, *want[i].retained)
			continue
		}
		if got := target.PartialPreemption.PodSet; got != victimPodSet {
			t.Errorf("target[%d] (%q): PodSet = %q, want %q", i, want[i].name, got, victimPodSet)
		}
		if got := target.PartialPreemption.RetainedCount; got != *want[i].retained {
			t.Errorf("target[%d] (%q): RetainedCount = %d, want %d", i, want[i].name, got, *want[i].retained)
		}
		if got := podSetRequestCount(target.PartialPreemption.ReducedInfo, victimPodSet); got != *want[i].retained {
			t.Errorf("target[%d] (%q): ReducedInfo executor count = %d, want %d",
				i, want[i].name, got, *want[i].retained)
		}
	}
}

func TestPartialPlanWalk(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.PartialPreemption, true)

	evicted := func(name string) *kueue.Workload {
		wl := partialPreemptionVictim(name, 0, 5, 1, nil)
		workloadevict.SetEvictedCondition(wl, time.Now(), kueue.WorkloadEvictedByPreemption, "test")
		return wl
	}
	e := evicted("e")
	e1 := evicted("e1")
	e2 := evicted("e2")
	// p is an in-flight partial-preemption victim (reclaim target 2, floor 1).
	p := partialPreemptionVictim("p", 0, 5, 1, new(int32(2)))
	s := partialPreemptionVictim("s", 0, 5, 1, nil)
	n := partialPreemptionVictim("n", 0, 5, 5, nil)

	cases := map[string]struct {
		// victims in candidate order.
		victims []*kueue.Workload
		// stopAfter makes the yield stop the walk once this many targets were seen.
		stopAfter int
		want      []wantStage
		wantCont  bool
	}{
		"walks the evicted, pending, partial and full phases in order": {
			victims: []*kueue.Workload{e, p, s, n},
			want: []wantStage{
				{name: "e"},
				{name: "p", retained: new(int32(1))},
				{name: "s", retained: new(int32(1))},
				{name: "p"},
				{name: "s"},
				{name: "n"},
			},
			wantCont: true,
		},
		"reports an interrupted walk": {
			victims:   []*kueue.Workload{e, p, s, n},
			stopAfter: 2,
			want: []wantStage{
				{name: "e"},
				{name: "p", retained: new(int32(1))},
			},
		},
		"whole-evicts the whole evicted prefix": {
			victims: []*kueue.Workload{e1, e2, s},
			want: []wantStage{
				{name: "e1"},
				{name: "e2"},
				{name: "s", retained: new(int32(1))},
				{name: "s"},
			},
			wantCont: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, pCtx := newPlanPreemptionCtx(t, "100", tc.victims, "1")
			candidates := make([]*workload.Info, 0, len(tc.victims))
			for _, victim := range tc.victims {
				info := pCtx.preemptorCQ.Workloads[workload.Key(victim)]
				if info == nil {
					t.Fatalf("victim %q missing from the snapshot", victim.Name)
				}
				candidates = append(candidates, info)
			}
			pl := &partialPlan{
				pCtx:     pCtx,
				iterator: &scriptedIterator{candidates: candidates, reason: "test"},
				ordering: byName,
			}
			var targets []*Target
			cont := true
			for target := range pl.walk() {
				targets = append(targets, target)
				if tc.stopAfter != 0 && len(targets) >= tc.stopAfter {
					cont = false
					break
				}
			}
			if cont != tc.wantCont {
				t.Errorf("walk() cont = %t, want %t", cont, tc.wantCont)
			}
			assertStages(t, targets, tc.want)
		})
	}
}

func TestFullPlanWalk(t *testing.T) {
	victims := []*kueue.Workload{
		partialPreemptionVictim("a", 0, 2, 2, nil),
		partialPreemptionVictim("b", 0, 2, 2, nil),
		partialPreemptionVictim("c", 0, 2, 2, nil),
	}
	cases := map[string]struct {
		stopAfter int
		want      []wantStage
		wantCont  bool
	}{
		"whole-evicts every candidate in candidate order": {
			want:     []wantStage{{name: "a"}, {name: "b"}, {name: "c"}},
			wantCont: true,
		},
		"reports an interrupted walk": {
			stopAfter: 1,
			want:      []wantStage{{name: "a"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, pCtx := newPlanPreemptionCtx(t, "100", victims, "1")
			candidates := make([]*workload.Info, 0, len(victims))
			for _, victim := range victims {
				info := pCtx.preemptorCQ.Workloads[workload.Key(victim)]
				if info == nil {
					t.Fatalf("victim %q missing from the snapshot", victim.Name)
				}
				candidates = append(candidates, info)
			}
			pl := &fullPlan{
				pCtx:     pCtx,
				iterator: &scriptedIterator{candidates: candidates, reason: "test"},
			}
			var targets []*Target
			cont := true
			for target := range pl.walk() {
				targets = append(targets, target)
				if tc.stopAfter != 0 && len(targets) >= tc.stopAfter {
					cont = false
					break
				}
			}
			if cont != tc.wantCont {
				t.Errorf("walk() cont = %t, want %t", cont, tc.wantCont)
			}
			assertStages(t, targets, tc.want)
		})
	}
}

func TestFullPlanFillBack(t *testing.T) {
	cases := map[string]struct {
		quota       string
		incomingCPU string
		// numTargets is how many of a, b, c fillBack receives; the last one is the
		// decisive removal and is never restored.
		numTargets int
		// wantNames are the surviving targets in candidate order.
		wantNames []string
		// wantPresent are the victims restored to the snapshot.
		wantPresent []string
	}{
		"a lone decisive victim is never restored": {
			quota: "6", incomingCPU: "3", numTargets: 1,
			wantNames:   []string{"a"},
			wantPresent: nil,
		},
		"keeps every victim when none fits back": {
			quota: "4", incomingCPU: "3", numTargets: 3,
			wantNames:   []string{"a", "b", "c"},
			wantPresent: nil,
		},
		"restores the victims the preemptor no longer needs": {
			quota: "6", incomingCPU: "3", numTargets: 3,
			wantNames:   []string{"a", "c"},
			wantPresent: []string{"b"},
		},
		"restores every earlier victim when the quota allows": {
			quota: "8", incomingCPU: "3", numTargets: 3,
			wantNames:   []string{"c"},
			wantPresent: []string{"a", "b"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			victims := []*kueue.Workload{
				partialPreemptionVictim("a", 0, 2, 2, nil),
				partialPreemptionVictim("b", 0, 2, 2, nil),
				partialPreemptionVictim("c", 0, 2, 2, nil),
			}
			victims = victims[:tc.numTargets]
			ctx, pCtx := newPlanPreemptionCtx(t, tc.quota, victims, tc.incomingCPU)

			var targets []*Target
			for _, victim := range victims {
				info := pCtx.preemptorCQ.Workloads[workload.Key(victim)]
				if info == nil {
					t.Fatalf("victim %q missing from the snapshot", victim.Name)
				}
				// Walk ended with a whole eviction of every victim.
				pCtx.snapshot.RemoveWorkload(info)
				targets = append(targets, wholeTarget(&Target{WorkloadInfo: info, Reason: "test"}, pCtx.preemptorCQ))
			}

			pl := &fullPlan{pCtx: pCtx}
			got := pl.fillBack(ctx, targets)

			gotNames := make([]string, len(got))
			for i := range got {
				gotNames[i] = got[i].WorkloadInfo.Obj.Name
			}
			if !slices.Equal(gotNames, tc.wantNames) {
				t.Errorf("fillBack() returned %v, want %v", gotNames, tc.wantNames)
			}
			wantPresent := make(map[string]bool, len(tc.wantPresent))
			for _, victimName := range tc.wantPresent {
				wantPresent[victimName] = true
			}
			for _, victim := range victims {
				_, present := snapshotExecutorCount(pCtx, victim)
				if wantPresent[victim.Name] && !present {
					t.Errorf("victim %q absent from the snapshot, want present", victim.Name)
				} else if !wantPresent[victim.Name] && present {
					t.Errorf("victim %q present in the snapshot, want absent", victim.Name)
				}
			}
		})
	}
}

func TestNewPlan(t *testing.T) {
	_, pCtx := newPlanPreemptionCtx(t, "6", nil, "1")
	iter := &scriptedIterator{}

	features.SetFeatureGateDuringTest(t, features.PartialPreemption, true)
	pl := newPlan(pCtx, iter, false, byName)
	if _, ok := pl.(*partialPlan); !ok {
		t.Errorf("newPlan() with PartialPreemption on = %T, want *partialPlan", pl)
	}

	features.SetFeatureGateDuringTest(t, features.PartialPreemption, false)
	pl = newPlan(pCtx, iter, false, byName)
	if _, ok := pl.(*fullPlan); !ok {
		t.Errorf("newPlan() with PartialPreemption off = %T, want *fullPlan", pl)
	}
}
