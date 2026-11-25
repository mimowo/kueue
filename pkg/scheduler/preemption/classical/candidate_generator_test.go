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
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clocktesting "k8s.io/utils/clock/testing"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func evictedInfo(t *testing.T, name string, evicted bool) *workload.Info {
	t.Helper()
	_, log := utiltesting.ContextWithLog(t)
	wl := utiltestingapi.MakeWorkload(name, "ns")
	if evicted {
		wl = wl.Condition(metav1.Condition{
			Type:               kueue.WorkloadEvicted,
			Status:             metav1.ConditionTrue,
			Reason:             "test",
			LastTransitionTime: metav1.NewTime(time.Now()),
		})
	}
	return workload.NewInfo(log, wl.Obj())
}

func byName(a, b *workload.Info) int {
	return strings.Compare(a.Obj.Name, b.Obj.Name)
}

func makeWorkloadCQ(infos []*workload.Info) *schdcache.ClusterQueueSnapshot {
	cq := &schdcache.ClusterQueueSnapshot{
		Workloads: make(map[workload.Reference]*workload.Info, len(infos)),
	}
	for _, wl := range infos {
		cq.Workloads[workload.Key(wl.Obj)] = wl
	}
	return cq
}

func segmentBucket(sortedWorkloads []*workload.Info, advantage bool) *bucket {
	b := &bucket{
		cq:                       makeWorkloadCQ(sortedWorkloads),
		isSameQueue:              true,
		hasHierarchicalAdvantage: advantage,
		sortedWorkloads:          sortedWorkloads,
	}
	return b
}

func drainIter(it *cacheCandidateIterator, borrow bool) []string {
	var got []string
	for {
		wl, _ := it.Next(borrow)
		if wl == nil {
			break
		}
		got = append(got, wl.Obj.Name)
	}
	return got
}

func TestCacheIterator(t *testing.T) {
	// shared workloads
	a := candidateInfo(t, "a", preemptorCQName, 1)
	b := candidateInfo(t, "b", preemptorCQName, 1)
	c := candidateInfo(t, "c", preemptorCQName, 1)
	d := candidateInfo(t, "d", preemptorCQName, 1)
	z := candidateInfo(t, "z", preemptorCQName, 1)
	real1 := candidateInfo(t, "real1", preemptorCQName, 1)
	real2 := candidateInfo(t, "real2", preemptorCQName, 1)
	fake := candidateInfo(t, "fake", preemptorCQName, 1)
	for i := range fake.TotalRequests {
		fake.TotalRequests[i].Flavors = nil
	}
	c1 := candidateInfo(t, "c1", otherCQName, 1)
	c2 := candidateInfo(t, "c2", otherCQName, 1)

	// ctxWithin: reclaim=Never, so cross-CQ candidates are not filtered by
	// policy; used for pure iteration-order cases.
	ctxWithin := preemptorCtx(t, 10, kueue.PreemptionPolicyAny, kueue.PreemptionPolicyNever, nil)
	// ctxReclaim: reclaim=Any but borrow forbidden; other-CQ candidates
	// classify as ReclaimWithoutBorrowing and are skipped on a borrow run.
	ctxReclaim := preemptorCtx(t, 10, kueue.PreemptionPolicyNever, kueue.PreemptionPolicyAny, nil)

	cases := map[string]struct {
		ctx      *HierarchicalPreemptionCtx
		segments [numSegments][]*bucket
		// first pass
		borrow bool
		want   []string // nil means expect no results
		// optional Reset + second pass
		doReset        bool
		resetBorrow    bool
		wantAfterReset []string
	}{
		"merges buckets from the same segment in comparator order": {
			ctx: ctxWithin,
			segments: func() [numSegments][]*bucket {
				var s [numSegments][]*bucket
				s[0] = []*bucket{
					segmentBucket([]*workload.Info{a, c}, false),
					segmentBucket([]*workload.Info{b, d}, false),
				}
				return s
			}(),
			borrow: false,
			want:   []string{"a", "b", "c", "d"},
		},
		"earlier segment always precedes a later segment regardless of element order": {
			// "z" sits in segment 2, "a" in segment 3: segment index must win over cmp.
			ctx: ctxWithin,
			segments: func() [numSegments][]*bucket {
				var s [numSegments][]*bucket
				s[2] = []*bucket{segmentBucket([]*workload.Info{z}, false)}
				s[3] = []*bucket{segmentBucket([]*workload.Info{a}, false)}
				return s
			}(),
			borrow: false,
			want:   []string{"z", "a"},
		},
		"Never candidates are silently skipped": {
			// fake uses no contested flavor -> classifies as Never; real1/real2 pass.
			ctx: ctxWithin,
			segments: func() [numSegments][]*bucket {
				var s [numSegments][]*bucket
				s[0] = []*bucket{segmentBucket([]*workload.Info{real1, fake, real2}, false)}
				return s
			}(),
			borrow: false,
			want:   []string{"real1", "real2"},
		},
		"workloads removed from the Snapshot are silently skipped": {
			ctx: ctxWithin,
			segments: func() [numSegments][]*bucket {
				var s [numSegments][]*bucket
				bkt := segmentBucket([]*workload.Info{a, b, c}, false)
				delete(bkt.cq.Workloads, workload.Key(b.Obj))
				s[0] = []*bucket{bkt}
				return s
			}(),
			borrow: false,
			want:   []string{"a", "c"},
		},
		"first workload removed from the Snapshot is skipped": {
			ctx: ctxWithin,
			segments: func() [numSegments][]*bucket {
				var s [numSegments][]*bucket
				bkt := segmentBucket([]*workload.Info{a, b, c}, false)
				delete(bkt.cq.Workloads, workload.Key(a.Obj))
				s[0] = []*bucket{bkt}
				return s
			}(),
			borrow: false,
			want:   []string{"b", "c"},
		},
		"bucket with all workloads removed from the Snapshot is exhausted": {
			ctx: ctxWithin,
			segments: func() [numSegments][]*bucket {
				var s [numSegments][]*bucket
				bkt := segmentBucket([]*workload.Info{a, b, c}, false)
				clear(bkt.cq.Workloads)
				s[0] = []*bucket{bkt}
				return s
			}(),
			borrow: false,
			want:   nil,
		},
		"borrow run skips ReclaimWithoutBorrowing; non-borrow run after Reset sees them": {
			ctx: ctxReclaim,
			segments: func() [numSegments][]*bucket {
				var s [numSegments][]*bucket
				s[0] = []*bucket{segmentBucket([]*workload.Info{c1, c2}, false)}
				return s
			}(),
			borrow:         true,
			want:           nil,
			doReset:        true,
			resetBorrow:    false,
			wantAfterReset: []string{"c1", "c2"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			it := &cacheCandidateIterator{segments: tc.segments, cmp: byName, hierarchicalReclaimCtx: tc.ctx}
			if got := drainIter(it, tc.borrow); !slices.Equal(got, tc.want) {
				t.Errorf("first pass = %v, want %v", got, tc.want)
			}
			if tc.doReset {
				it.Reset()
				if got := drainIter(it, tc.resetBorrow); !slices.Equal(got, tc.wantAfterReset) {
					t.Errorf("after Reset = %v, want %v", got, tc.wantAfterReset)
				}
			}
		})
	}
}

func TestResetSkipsWorkloadsRemovedFromSnapshot(t *testing.T) {
	cases := map[string]struct {
		removed []string
		want    []string
	}{
		"first workload": {
			removed: []string{"a"},
			want:    []string{"b", "c"},
		},
		"middle workload": {
			removed: []string{"b"},
			want:    []string{"a", "c"},
		},
		"last workload": {
			removed: []string{"c"},
			want:    []string{"a", "b"},
		},
		"all workloads": {
			removed: []string{"a", "b", "c"},
			want:    nil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			workloads := []*workload.Info{
				candidateInfo(t, "a", preemptorCQName, 1),
				candidateInfo(t, "b", preemptorCQName, 1),
				candidateInfo(t, "c", preemptorCQName, 1),
			}
			workloadsByName := make(map[string]*workload.Info, len(workloads))
			for _, wl := range workloads {
				workloadsByName[wl.Obj.Name] = wl
			}
			bkt := segmentBucket(workloads, false)
			it := &cacheCandidateIterator{
				segments:               [numSegments][]*bucket{{bkt}},
				cmp:                    byName,
				hierarchicalReclaimCtx: preemptorCtx(t, 10, kueue.PreemptionPolicyAny, kueue.PreemptionPolicyNever, nil),
			}

			if got, want := drainIter(it, false), []string{"a", "b", "c"}; !slices.Equal(got, want) {
				t.Fatalf("first pass = %v, want %v", got, want)
			}

			for _, removed := range tc.removed {
				delete(bkt.cq.Workloads, workload.Key(workloadsByName[removed].Obj))
			}
			it.Reset()
			if got := drainIter(it, false); !slices.Equal(got, tc.want) {
				t.Errorf("after Reset = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBucketCursor(t *testing.T) {
	w := []*workload.Info{
		{Obj: utiltestingapi.MakeWorkload("a", "ns").Obj()},
		{Obj: utiltestingapi.MakeWorkload("b", "ns").Obj()},
		{Obj: utiltestingapi.MakeWorkload("c", "ns").Obj()},
	}

	t.Run("forward consumption", func(t *testing.T) {
		b := segmentBucket(w, false)
		got := []string{}
		for !b.empty() {
			got = append(got, b.top().Obj.Name)
			b.advance()
		}
		if !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Errorf("consumed %v, want [a b c]", got)
		}
		if !b.empty() {
			t.Error("bucket should be empty after full consumption")
		}
	})

	t.Run("empty slice", func(t *testing.T) {
		b := segmentBucket(nil, false)
		if !b.empty() {
			t.Error("nil-backed bucket should be empty")
		}
	})

	t.Run("drain exhausts immediately", func(t *testing.T) {
		b := segmentBucket(w, false)
		if b.empty() {
			t.Fatal("bucket should start non-empty")
		}
		b.drain()
		if !b.empty() {
			t.Error("bucket should be empty after drain()")
		}
	})

	t.Run("reset rewinds after cursor movement", func(t *testing.T) {
		b := segmentBucket(w, false)
		b.advance()
		b.advance()
		b.reset()
		if b.empty() || b.top().Obj.Name != "a" {
			t.Errorf("after reset top should be a, got empty=%v", b.empty())
		}
	})

	t.Run("reset rewinds after drain", func(t *testing.T) {
		b := segmentBucket(w, false)
		b.drain()
		b.reset()
		if b.empty() || b.top().Obj.Name != "a" {
			t.Error("reset must rewind a drained bucket so it participates in the next run")
		}
	})
}

func TestBuildBuckets(t *testing.T) {
	e := evictedInfo(t, "e", true)
	n1 := evictedInfo(t, "n1", false)
	n2 := evictedInfo(t, "n2", false)

	cqA := segmentBucket([]*workload.Info{e, n1}, false).cq
	cqA.Name = "cqA"
	cqB := segmentBucket([]*workload.Info{n2}, false).cq
	cqB.Name = "cqB"
	cqEmpty := segmentBucket(nil, false).cq
	cqEmpty.Name = "cqEmpty"

	cases := map[string]struct {
		classes           []classifiedClusterQueue
		wantEvicted       int
		wantNonEvicted    int
		wantEvictedCQ     string
		wantEvictedTop    string
		wantNonEvictedCQs []string
	}{
		"evicted bucket, non-evicted bucket, empty CQ skipped": {
			classes: []classifiedClusterQueue{
				// cqA has both an evicted prefix (evictedCount=1) and a non-evicted tail.
				// buildBuckets splits at the cache-computed evictedCount, not by re-probing
				// the workload conditions.
				{cq: cqA, sortedWorkloads: []*workload.Info{e, n1}, evictedCount: 1},
				// cqB has only a non-evicted workload.
				{cq: cqB, sortedWorkloads: []*workload.Info{n2}, evictedCount: 0},
				// cqEmpty contributes nothing.
				{cq: cqEmpty, sortedWorkloads: nil},
			},
			wantEvicted:       1,
			wantNonEvicted:    2,
			wantEvictedCQ:     "cqA",
			wantEvictedTop:    "e",
			wantNonEvictedCQs: []string{"cqA", "cqB"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			evicted, nonEvicted := buildBuckets(tc.classes)

			if len(evicted) != tc.wantEvicted {
				t.Fatalf("evicted buckets = %d, want %d", len(evicted), tc.wantEvicted)
			}
			if tc.wantEvicted > 0 {
				if evicted[0].cq.Name != kueue.ClusterQueueReference(tc.wantEvictedCQ) || evicted[0].top().Obj.Name != tc.wantEvictedTop {
					t.Errorf("evicted bucket = (%q, %q), want (%q, %q)", evicted[0].cq.Name, evicted[0].top().Obj.Name, tc.wantEvictedCQ, tc.wantEvictedTop)
				}
			}

			if len(nonEvicted) != tc.wantNonEvicted {
				t.Fatalf("non-evicted buckets = %d, want %d", len(nonEvicted), tc.wantNonEvicted)
			}
			gotCQs := make([]string, len(nonEvicted))
			for i, b := range nonEvicted {
				gotCQs[i] = string(b.cq.Name)
			}
			if !slices.Equal(gotCQs, tc.wantNonEvictedCQs) {
				t.Errorf("non-evicted bucket CQs = %v, want %v", gotCQs, tc.wantNonEvictedCQs)
			}
		})
	}
}

func TestCandidateIteratorBands(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	frs := sets.New(resources.FlavorResource{Flavor: testFlavor, Resource: corev1.ResourceCPU})

	makeCQ := func(name, cohort, nominal string, preemption kueue.ClusterQueuePreemption) *kueue.ClusterQueue {
		b := utiltestingapi.MakeClusterQueue(name).
			ResourceGroup(*utiltestingapi.MakeFlavorQuotas(testFlavor).Resource(corev1.ResourceCPU, nominal).Obj()).
			Preemption(preemption)
		if cohort != "" {
			b = b.Cohort(kueue.CohortReference(cohort))
		}
		return b.Obj()
	}

	makeWl := func(name, cq string, prio int32, cpus string) *kueue.Workload {
		return utiltestingapi.MakeWorkload(name, "ns").
			UID(types.UID(name)).
			Queue(kueue.LocalQueueName(name)).
			Priority(prio).
			Request(corev1.ResourceCPU, cpus).
			SimpleReserveQuota(kueue.ClusterQueueReference(cq), testFlavor, now).
			Obj()
	}

	cohortPreempt := kueue.ClusterQueuePreemption{
		WithinClusterQueue:  kueue.PreemptionPolicyLowerPriority,
		ReclaimWithinCohort: kueue.PreemptionPolicyAny,
		BorrowWithinCohort:  &kueue.BorrowWithinCohort{Policy: kueue.BorrowWithinCohortPolicyLowerPriority},
	}

	cases := map[string]struct {
		cqs          []*kueue.ClusterQueue
		workloads    []*kueue.Workload
		enabledAfs   bool
		afsUsage     map[string]float64
		preemptorCQ  kueue.ClusterQueueReference
		preemptorCPU string
		borrow       bool
		wantBands    [][]string
	}{
		"segments and priority tiers": {
			cqs: []*kueue.ClusterQueue{
				makeCQ("p", "coh", "4", cohortPreempt),
				makeCQ("q", "coh", "2", cohortPreempt),
			},
			workloads: []*kueue.Workload{
				makeWl("own-low", "p", 0, "1"),
				makeWl("own-high", "p", 10, "1"),
				makeWl("other-low", "q", 0, "2"),
				makeWl("other-high", "q", 10, "2"),
				func() *kueue.Workload {
					wl := makeWl("other-evicted", "q", 20, "1")
					wl.Status.Conditions = append(wl.Status.Conditions, metav1.Condition{
						Type:               kueue.WorkloadEvicted,
						Status:             metav1.ConditionTrue,
						Reason:             "Preempted",
						LastTransitionTime: metav1.NewTime(now),
					})
					return wl
				}(),
			},
			preemptorCQ:  "p",
			preemptorCPU: "2",
			borrow:       true,
			wantBands: [][]string{
				{"other-evicted"},
				{"other-low"},
				{"other-high"},
				{"own-low"},
				{"own-high"},
			},
		},
		"cross-CQ equal-priority peers share one band": {
			cqs: []*kueue.ClusterQueue{
				makeCQ("p", "coh", "6", cohortPreempt),
				makeCQ("q", "coh", "2", cohortPreempt),
				makeCQ("r", "coh", "2", cohortPreempt),
			},
			workloads: []*kueue.Workload{
				makeWl("a", "q", 0, "4"),
				makeWl("b", "r", 0, "4"),
				makeWl("c", "q", 5, "1"),
			},
			preemptorCQ:  "p",
			preemptorCPU: "4",
			borrow:       true,
			wantBands: [][]string{
				{"a", "b"},
				{"c"},
			},
		},
		"AFS tiers do not chain across another ClusterQueue": {
			cqs: []*kueue.ClusterQueue{
				makeCQ("p", "coh", "10", cohortPreempt),
				makeCQ("q", "coh", "2", cohortPreempt),
				makeCQ("r", "coh", "2", cohortPreempt),
			},
			workloads: []*kueue.Workload{
				makeWl("a", "q", 0, "4"),
				makeWl("b", "r", 0, "4"),
				makeWl("c", "q", 0, "4"),
			},
			enabledAfs:   true,
			afsUsage:     map[string]float64{"a": 100, "b": 30, "c": 1},
			preemptorCQ:  "p",
			preemptorCPU: "4",
			borrow:       true,
			wantBands: [][]string{
				{"a", "b"},
				{"c"},
			},
		},
	}

	for _, cacheEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cacheEnabled), func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.PartialPreemption, true)
			features.SetFeatureGateDuringTest(t, features.ClassicalPreemptionCandidateOrderCache, cacheEnabled)
			for name, tc := range cases {
				t.Run(name, func(t *testing.T) {
					ctx, log := utiltesting.ContextWithLog(t)
					items := make([]kueue.Workload, len(tc.workloads))
					for i, wl := range tc.workloads {
						items[i] = *wl
					}
					cl := utiltesting.NewClientBuilder().WithLists(&kueue.WorkloadList{Items: items}).Build()
					cqCache := schdcache.New(cl)
					cqCache.AddOrUpdateResourceFlavor(log, utiltestingapi.MakeResourceFlavor(testFlavor).Obj())
					for _, cq := range tc.cqs {
						if err := cqCache.AddClusterQueue(ctx, cq); err != nil {
							t.Fatalf("AddClusterQueue(%q): %v", cq.Name, err)
						}
					}
					snap, err := cqCache.Snapshot(ctx)
					if err != nil {
						t.Fatalf("Snapshot: %v", err)
					}
					for _, cqSnap := range snap.ClusterQueues() {
						for key, wl := range cqSnap.Workloads {
							if usage, ok := tc.afsUsage[wl.Obj.Name]; ok {
								wlCopy := *wl
								wlCopy.LocalQueueFSUsage = new(usage)
								cqSnap.Workloads[key] = &wlCopy
							}
						}
					}

					preemptorWl := utiltestingapi.MakeWorkload("in", "ns").Priority(100).Request(corev1.ResourceCPU, tc.preemptorCPU).Obj()
					preemptorInfo := workload.NewInfo(log, preemptorWl)
					hCtx := &HierarchicalPreemptionCtx{
						Log:               log,
						Wl:                preemptorWl,
						Cq:                snap.ClusterQueue(tc.preemptorCQ),
						FrsNeedPreemption: frs,
						Requests: resources.FlavorResourceQuantities{
							{Flavor: testFlavor, Resource: corev1.ResourceCPU}: preemptorInfo.TotalRequests[0].Requests.ResourceValue(corev1.ResourceCPU),
						},
					}
					clk := clocktesting.NewFakeClock(now)
					it := NewCandidateIterator(hCtx, tc.enabledAfs, frs, snap, clk, policy.CandidatesOrdering)

					var gotBands [][]string
					for band := range it.Bands(tc.borrow) {
						var names []string
						for wl := range band.Candidates {
							names = append(names, wl.Obj.Name)
						}
						gotBands = append(gotBands, names)
					}
					if diff := cmp.Diff(tc.wantBands, gotBands); diff != "" {
						t.Errorf("Bands() (-want,+got):\n%s", diff)
					}
				})
			}
		})
	}
}

func TestCandidateIteratorBandLiveEligibilityAndInProgress(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	frs := sets.New(resources.FlavorResource{Flavor: testFlavor, Resource: corev1.ResourceCPU})
	cohortPreempt := kueue.ClusterQueuePreemption{
		WithinClusterQueue:  kueue.PreemptionPolicyLowerPriority,
		ReclaimWithinCohort: kueue.PreemptionPolicyAny,
		BorrowWithinCohort:  &kueue.BorrowWithinCohort{Policy: kueue.BorrowWithinCohortPolicyLowerPriority},
	}

	for _, cacheEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cacheEnabled), func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.PartialPreemption, true)
			features.SetFeatureGateDuringTest(t, features.ClassicalPreemptionCandidateOrderCache, cacheEnabled)
			ctx, log := utiltesting.ContextWithLog(t)

			a := utiltestingapi.MakeWorkload("a", "ns").
				UID("a").
				Priority(0).
				Annotation(constants.PartialPreemptionAnnotation, "true").
				PodSets(*utiltestingapi.MakePodSet("main", 3).Request(corev1.ResourceCPU, "1").PartialPreemptionMinRetainedCount(1).Obj()).
				SimpleReserveQuota("q", testFlavor, now).
				Obj()
			b := utiltestingapi.MakeWorkload("b", "ns").
				UID("b").
				Priority(0).
				Annotation(constants.PartialPreemptionAnnotation, "true").
				PodSets(*utiltestingapi.MakePodSet("main", 3).Request(corev1.ResourceCPU, "1").PartialPreemptionMinRetainedCount(1).Obj()).
				SimpleReserveQuota("q", testFlavor, now).
				Obj()
			pendingHigh := utiltestingapi.MakeWorkload("pending-high", "ns").
				UID("pending-high").
				Priority(20).
				Annotation(constants.PartialPreemptionAnnotation, "true").
				PodSets(*utiltestingapi.MakePodSet("main", 4).Request(corev1.ResourceCPU, "1").PartialPreemptionMinRetainedCount(1).Obj()).
				ReserveQuotaAt(utiltestingapi.MakeAdmission("p").
					PodSets(utiltestingapi.MakePodSetAssignment("main").
						Assignment(corev1.ResourceCPU, testFlavor, "4").
						Count(4).
						PartialPreemptionRetainedCount(2).
						Obj()).
					Obj(), now).
				Obj()
			evictedHigh := utiltestingapi.MakeWorkload("evicted-high", "ns").
				UID("evicted-high").
				Priority(30).
				Request(corev1.ResourceCPU, "1").
				SimpleReserveQuota("p", testFlavor, now).
				EvictedAt(now).
				Obj()

			cqs := []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("p").Cohort("coh").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(testFlavor).Resource(corev1.ResourceCPU, "6").Obj()).
					Preemption(cohortPreempt).Obj(),
				utiltestingapi.MakeClusterQueue("q").Cohort("coh").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(testFlavor).Resource(corev1.ResourceCPU, "4").Obj()).
					Preemption(cohortPreempt).Obj(),
			}
			cl := utiltesting.NewClientBuilder().WithLists(&kueue.WorkloadList{Items: []kueue.Workload{*a, *b, *pendingHigh, *evictedHigh}}).Build()
			cqCache := schdcache.New(cl)
			cqCache.AddOrUpdateResourceFlavor(log, utiltestingapi.MakeResourceFlavor(testFlavor).Obj())
			for _, cq := range cqs {
				if err := cqCache.AddClusterQueue(ctx, cq); err != nil {
					t.Fatalf("AddClusterQueue(%q): %v", cq.Name, err)
				}
			}
			snap, err := cqCache.Snapshot(ctx)
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}

			preemptorWl := utiltestingapi.MakeWorkload("in", "ns").Priority(100).Request(corev1.ResourceCPU, "4").Obj()
			preemptorInfo := workload.NewInfo(log, preemptorWl)
			hCtx := &HierarchicalPreemptionCtx{
				Log:               log,
				Wl:                preemptorWl,
				Cq:                snap.ClusterQueue("p"),
				FrsNeedPreemption: frs,
				Requests: resources.FlavorResourceQuantities{
					{Flavor: testFlavor, Resource: corev1.ResourceCPU}: preemptorInfo.TotalRequests[0].Requests.ResourceValue(corev1.ResourceCPU),
				},
			}
			clk := clocktesting.NewFakeClock(now)
			it := NewCandidateIterator(hCtx, false, frs, snap, clk, policy.CandidatesOrdering)

			// InProgressCandidates yields the workload that is being evicted before
			// the one that is shrinking.
			var gotInProg []string
			for wl := range it.InProgressCandidates(true) {
				gotInProg = append(gotInProg, wl.Obj.Name)
			}
			if diff := cmp.Diff([]string{"evicted-high", "pending-high"}, gotInProg); diff != "" {
				t.Errorf("InProgressCandidates() (-want,+got):\n%s", diff)
			}

			// The band {a, b} is in ClusterQueue q (nominal 4, usage 6). Removing a
			// during a walk of Candidates brings q to 3 <= 4, which protects b.
			// Once a is back, the next walk has to start the band over and offer
			// both a and b, and it has to stop offering b once a is removed.
			var checkedBand bool
			for band := range it.Bands(true) {
				var shrinkNames []string
				var aInfo *workload.Info
				for wl := range band.Candidates {
					shrinkNames = append(shrinkNames, wl.Obj.Name)
					if wl.Obj.Name == "a" {
						aInfo = wl
						snap.RemoveWorkload(wl)
					}
				}
				if aInfo == nil {
					continue
				}
				checkedBand = true
				if diff := cmp.Diff([]string{"a"}, shrinkNames); diff != "" {
					t.Errorf("Candidates while a is removed (-want,+got):\n%s", diff)
				}
				snap.AddWorkload(aInfo)
				var rewindNames []string
				var bInfo *workload.Info
				for wl := range band.Candidates {
					rewindNames = append(rewindNames, wl.Obj.Name)
					if wl.Obj.Name == "b" {
						bInfo = wl
					}
				}
				if diff := cmp.Diff([]string{"a", "b"}, rewindNames); diff != "" {
					t.Errorf("Candidates after restoring a (-want,+got):\n%s", diff)
				}
				var evictNames []string
				for wl := range band.Candidates {
					evictNames = append(evictNames, wl.Obj.Name)
					snap.RemoveWorkload(wl)
				}
				if diff := cmp.Diff([]string{"a"}, evictNames); diff != "" {
					t.Errorf("Candidates while evicting a (-want,+got):\n%s", diff)
				}
				if bInfo == nil || band.IsEligible(bInfo) {
					t.Errorf("IsEligible(b) = true after CQ q dropped within nominal, want false")
				}
				break
			}
			if !checkedBand {
				t.Fatalf("did not encounter expected {a, b} band")
			}
		})
	}
}

func TestCandidateIteratorBandEligibilityScope(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	frs := sets.New(resources.FlavorResource{Flavor: testFlavor, Resource: corev1.ResourceCPU})
	makeCQ := func(name, cohort, nominal string) *kueue.ClusterQueue {
		return utiltestingapi.MakeClusterQueue(name).
			Cohort(kueue.CohortReference(cohort)).
			ResourceGroup(*utiltestingapi.MakeFlavorQuotas(testFlavor).Resource(corev1.ResourceCPU, nominal).Obj()).
			Preemption(kueue.ClusterQueuePreemption{
				WithinClusterQueue:  kueue.PreemptionPolicyLowerPriority,
				ReclaimWithinCohort: kueue.PreemptionPolicyAny,
			}).
			Obj()
	}
	makeWl := func(name, cq string) kueue.Workload {
		return *utiltestingapi.MakeWorkload(name, "ns").
			UID(types.UID(name)).
			Priority(0).
			Request(corev1.ResourceCPU, "1").
			SimpleReserveQuota(kueue.ClusterQueueReference(cq), testFlavor, now).
			Obj()
	}

	for _, cacheEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cacheEnabled), func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.ClassicalPreemptionCandidateOrderCache, cacheEnabled)
			ctx, log := utiltesting.ContextWithLog(t)

			// ClusterQueues q1 and q2 are in the cohort "branch". The cohort "root"
			// holds "branch", the preempting ClusterQueue p and ClusterQueue r.
			workloads := []kueue.Workload{makeWl("own", "p"), makeWl("a", "q1"), makeWl("b", "q2"), makeWl("c", "r")}
			cl := utiltesting.NewClientBuilder().WithLists(&kueue.WorkloadList{Items: workloads}).Build()
			cqCache := schdcache.New(cl)
			cqCache.AddOrUpdateResourceFlavor(log, utiltestingapi.MakeResourceFlavor(testFlavor).Obj())
			if err := cqCache.AddOrUpdateCohort(utiltestingapi.MakeCohort("branch").Parent("root").Obj()); err != nil {
				t.Fatalf("AddOrUpdateCohort: %v", err)
			}
			for _, cq := range []*kueue.ClusterQueue{
				makeCQ("p", "root", "5"),
				makeCQ("q1", "branch", "0"),
				makeCQ("q2", "branch", "0"),
				makeCQ("r", "root", "0"),
			} {
				if err := cqCache.AddClusterQueue(ctx, cq); err != nil {
					t.Fatalf("AddClusterQueue(%q): %v", cq.Name, err)
				}
			}
			snap, err := cqCache.Snapshot(ctx)
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}

			preemptorWl := utiltestingapi.MakeWorkload("in", "ns").Priority(100).Request(corev1.ResourceCPU, "5").Obj()
			hCtx := &HierarchicalPreemptionCtx{
				Log:               log,
				Wl:                preemptorWl,
				Cq:                snap.ClusterQueue("p"),
				FrsNeedPreemption: frs,
				Requests: resources.FlavorResourceQuantities{
					{Flavor: testFlavor, Resource: corev1.ResourceCPU}: workload.NewInfo(log, preemptorWl).TotalRequests[0].Requests.ResourceValue(corev1.ResourceCPU),
				},
			}
			it := NewCandidateIterator(hCtx, false, frs, snap, clocktesting.NewFakeClock(now), policy.CandidatesOrdering)

			gotScopes := make(map[string]string)
			for band := range it.Bands(false) {
				for wl := range band.Candidates {
					switch scope := band.EligibilityScope(wl).(type) {
					case nil:
						gotScopes[wl.Obj.Name] = ""
					case *schdcache.ClusterQueueSnapshot:
						gotScopes[wl.Obj.Name] = "ClusterQueue " + string(scope.Name)
					case *schdcache.CohortSnapshot:
						gotScopes[wl.Obj.Name] = "Cohort " + string(scope.Name)
					}
				}
			}
			wantScopes := map[string]string{
				"own": "",
				"a":   "Cohort branch",
				"b":   "Cohort branch",
				"c":   "ClusterQueue r",
			}
			if diff := cmp.Diff(wantScopes, gotScopes); diff != "" {
				t.Errorf("EligibilityScope() by candidate (-want,+got):\n%s", diff)
			}
		})
	}
}
