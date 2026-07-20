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

package tas

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// TestPrefixRunsOptimizedTopologyAssignmentEncoding verifies equivalence with
// prefix-runs-first, including assignments for which catalog run selection does
// not apply.
func TestPrefixRunsOptimizedTopologyAssignmentEncoding(t *testing.T) {
	tests := map[string]*TopologyAssignment{
		"empty": {},
		"non-hostname levels": {
			Levels: []string{"cloud.example.com/zone"},
			Domains: []TopologyDomainAssignment{
				{Values: []string{"zone-a"}, Count: 2},
				{Values: []string{"zone-b"}, Count: 3},
			},
		},
		"single hostname": {
			Levels:  []string{corev1.LabelHostname},
			Domains: []TopologyDomainAssignment{{Values: []string{"pool-a-0"}, Count: 1}},
		},
		"unsorted hostname runs": {
			Levels: []string{corev1.LabelHostname},
			Domains: []TopologyDomainAssignment{
				{Values: []string{"pool-b-0"}, Count: 1},
				{Values: []string{"pool-b-1"}, Count: 2},
				{Values: []string{"pool-a-0"}, Count: 3},
				{Values: []string{"pool-a-1"}, Count: 4},
			},
		},
		"multiple levels": {
			Levels: []string{"cloud.example.com/rack", corev1.LabelHostname},
			Domains: []TopologyDomainAssignment{
				{Values: []string{"rack-b", "pool-b-0"}, Count: 1},
				{Values: []string{"rack-b", "pool-b-1"}, Count: 2},
				{Values: []string{"rack-a", "pool-a-0"}, Count: 3},
				{Values: []string{"rack-a", "pool-a-1"}, Count: 4},
			},
		},
	}
	for name, ta := range tests {
		t.Run(name, func(t *testing.T) {
			want := compactTopologyAssignmentEncodingWithHostnamePrefixRuns(ta)
			got := prefixRunsOptimizedTopologyAssignmentEncoding(ta)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("unexpected catalog encoding (-want,+got):\n%s", diff)
			}
		})
	}
}

// TestHostnamePrefixCatalogDocumentedExample keeps the catalog structure and
// reusable-prefix lookup examples synchronized with their comments.
func TestHostnamePrefixCatalogDocumentedExample(t *testing.T) {
	ta := topologyAssignmentForHostnames([]string{
		"pool-a-0",
		"pool-b-0",
		"pool-a-1",
		"batch-x-0",
		"batch-x-1",
	}, false)
	catalog := newHostnamePrefixCatalog(ta)
	wantCatalog := &hostnamePrefixCatalog{
		offsets:          []uint32{0, 2, 4, 6, 8, 10},
		ids:              []hostnamePrefixID{0, 1, 0, 2, 0, 1, 3, 4, 3, 4},
		endsByID:         []uint16{5, 7, 7, 6, 8},
		countsByID:       []uint8{2, 2, 1, 2, 2},
		sentinelPrefixID: 5,
		maxDepth:         2,
	}
	if diff := cmp.Diff(wantCatalog, catalog, cmp.AllowUnexported(hostnamePrefixCatalog{})); diff != "" {
		t.Fatalf("unexpected catalog (-want,+got):\n%s", diff)
	}
	if diff := cmp.Diff([]hostnamePrefixID{0, 1}, catalog.prefixIDsAt(2)); diff != "" {
		t.Errorf("unexpected domain prefix IDs (-want,+got):\n%s", diff)
	}
	if got := catalog.reusablePrefixIDAt(2, 2); got != 1 {
		t.Errorf("unexpected deepest reusable prefix: got %d, want 1", got)
	}
	if got := catalog.reusablePrefixIDAt(1, 2); got != 0 {
		t.Errorf("unexpected fallback reusable prefix: got %d, want 0", got)
	}

	uniqueTA := topologyAssignmentForHostnames([]string{"node-a", "host-b"}, false)
	uniqueCatalog := newHostnamePrefixCatalog(uniqueTA)
	if got, want := uniqueCatalog.reusablePrefixIDAt(0, 1), uniqueCatalog.sentinelPrefixID; got != want {
		t.Errorf("unexpected absent-prefix ID: got %d, want %d", got, want)
	}
}

const fuzzHostnameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// randomFuzzHostnames generates unique random hostnames whose individual
// lengths vary from 10 through maxLength and whose individual '-' counts vary
// from zero through maxHyphenCount, further bounded so '-' is never the first
// or last character. With maxLength=30 and maxHyphenCount=14, one assignment
// therefore exercises multiple hostname lengths and prefix depths. Expected
// time and space complexity are O(n*m).
func randomFuzzHostnames(nodeCount, maxLength, maxHyphenCount int, seed uint64) []string {
	hostnames := make([]string, nodeCount)
	seen := make(map[string]struct{}, nodeCount)
	rnd := rand.New(rand.NewPCG(seed, seed^0x72616e646f6d))
	for nodeIndex := range nodeCount {
		for {
			length := 10 + rnd.IntN(maxLength-9)
			hostname := make([]byte, length)
			for position := range hostname {
				hostname[position] = fuzzHostnameAlphabet[rnd.IntN(len(fuzzHostnameAlphabet))]
			}
			hyphenCount := rnd.IntN(min(maxHyphenCount, length-2) + 1)
			for inserted := 0; inserted < hyphenCount; {
				position := 1 + rnd.IntN(length-2)
				if hostname[position] == '-' {
					continue
				}
				hostname[position] = '-'
				inserted++
			}

			generated := string(hostname)
			if _, duplicate := seen[generated]; duplicate {
				continue
			}
			seen[generated] = struct{}{}
			hostnames[nodeIndex] = generated
			break
		}
	}
	return hostnames
}

// TestRandomFuzzHostnames keeps the random-mode bounds synchronized with the
// round-trip fuzz normalization and verifies that one generated assignment
// actually contains multiple hostname lengths and '-' counts.
func TestRandomFuzzHostnames(t *testing.T) {
	hostnames := randomFuzzHostnames(1_000, 30, 14, 1)
	seen := make(map[string]struct{}, len(hostnames))
	lengths := make(map[int]struct{})
	hyphenCounts := make(map[int]struct{})
	for _, hostname := range hostnames {
		if len(hostname) < 10 || len(hostname) > 30 {
			t.Errorf("hostname %q has length %d, want [10,30]", hostname, len(hostname))
		}
		if hostname[0] == '-' || hostname[len(hostname)-1] == '-' {
			t.Errorf("hostname %q has a leading or trailing hyphen", hostname)
		}
		if _, duplicate := seen[hostname]; duplicate {
			t.Errorf("duplicate hostname %q", hostname)
		}
		seen[hostname] = struct{}{}
		lengths[len(hostname)] = struct{}{}
		hyphenCounts[strings.Count(hostname, "-")] = struct{}{}
	}
	if len(lengths) < 2 {
		t.Errorf("generated only one hostname length: %v", lengths)
	}
	if len(hyphenCounts) < 2 {
		t.Errorf("generated only one hyphen count: %v", hyphenCounts)
	}
}

// hostnamePrefixAssignmentParameters names the normalized inputs used to
// generate one fuzz assignment.
type hostnamePrefixAssignmentParameters struct {
	nodeCount         int
	groupCount        int
	prefixDepth       int
	segmentWidth      int
	seed              uint64
	multiLevel        bool
	randomNames       bool
	maxHostnameLength int
	maxHyphenCount    int
}

// fuzzHostnamePrefixAssignment transforms named, bounded parameters into an
// arbitrarily ordered assignment containing either all generated hostnames or
// a subset. Structured mode generates shared prefix hierarchies; random mode
// varies hostname length and '-' count independently per node.
//
// For example, nodeCount=4, groupCount=2, prefixDepth=1, and segmentWidth=2
// generate the catalog hostnames ["nn-00-0000", "nn-01-0001",
// "nn-00-0002", "nn-01-0003"] before any seed-controlled position shuffle.
//
// Time and space complexity are O(n*m), dominated by hostname generation.
func fuzzHostnamePrefixAssignment(parameters hostnamePrefixAssignmentParameters) *TopologyAssignment {
	nodeCount := parameters.nodeCount
	hostnames := make([]string, nodeCount)
	if parameters.randomNames {
		hostnames = randomFuzzHostnames(nodeCount, parameters.maxHostnameLength, parameters.maxHyphenCount, parameters.seed)
	} else {
		for i := range nodeCount {
			base := strings.Repeat("n", parameters.segmentWidth)
			if parameters.prefixDepth == 0 {
				hostnames[i] = fmt.Sprintf("%s%04x", base, i)
				continue
			}

			segments := make([]string, 1, parameters.prefixDepth+2)
			segments[0] = base
			for depth := range parameters.prefixDepth {
				groupsAtDepth := min(nodeCount, parameters.groupCount<<depth)
				segments = append(segments, fmt.Sprintf("%0*x", parameters.segmentWidth, i%groupsAtDepth))
			}
			segments = append(segments, fmt.Sprintf("%04x", i))
			hostnames[i] = strings.Join(segments, "-")
		}
	}

	order := make([]int, nodeCount)
	for i := range order {
		order[i] = i
	}
	rnd := rand.New(rand.NewPCG(parameters.seed, parameters.seed^0x9e3779b97f4a7c15))
	if parameters.seed != 0 {
		rnd.Shuffle(len(order), func(i, j int) {
			order[i], order[j] = order[j], order[i]
		})
	}
	if len(order) > 1 && parameters.seed&1 != 0 {
		order = order[:1+rnd.IntN(len(order))]
	}

	levels := []string{corev1.LabelHostname}
	if parameters.multiLevel {
		levels = []string{"cloud.example.com/rack", corev1.LabelHostname}
	}
	domains := make([]TopologyDomainAssignment, len(order))
	for i, hostnameIndex := range order {
		values := []string{hostnames[hostnameIndex]}
		if parameters.multiLevel {
			values = []string{fmt.Sprintf("rack-%02x", hostnameIndex%17), hostnames[hostnameIndex]}
		}
		domains[i] = TopologyDomainAssignment{
			Values: values,
			Count:  int32(1 + rnd.IntN(5)),
		}
	}
	return &TopologyAssignment{Levels: levels, Domains: domains}
}

const (
	// maxNodeCount bounds fuzz-generated assignments while allowing correctness
	// coverage beyond both topology-assignment slice limits.
	maxNodeCount = 120_000

	// smallRandomizedMaxNodeCount keeps most deterministic randomized cases
	// small enough to preserve iteration breadth.
	smallRandomizedMaxNodeCount = 3 * maxTopologyAssignmentSlices
)

// hostnamePrefixFuzzParameters names the primitive values accepted by the Go
// fuzzing API. Values remain raw so mutation can explore the complete integer
// ranges before verifyHostnamePrefixCatalogJSONRoundTrip bounds them.
type hostnamePrefixFuzzParameters struct {
	rawNodeCount         uint32
	rawGroupCount        uint16
	rawPrefixDepth       uint8
	rawSegmentWidth      uint8
	seed                 uint64
	rawMultiLevel        uint8
	rawRandomNames       uint8
	rawMaxHostnameLength uint8
	rawMaxHyphenCount    uint8
}

// addHostnamePrefixFuzzSeed is the only positional bridge to testing.F.Add,
// whose corpus API does not accept structs. Callers use named struct fields so
// the meaning of each seed value remains visible.
func addHostnamePrefixFuzzSeed(f *testing.F, parameters hostnamePrefixFuzzParameters) {
	f.Helper()
	f.Add(
		parameters.rawNodeCount,
		parameters.rawGroupCount,
		parameters.rawPrefixDepth,
		parameters.rawSegmentWidth,
		parameters.seed,
		parameters.rawMultiLevel,
		parameters.rawRandomNames,
		parameters.rawMaxHostnameLength,
		parameters.rawMaxHyphenCount,
	)
}

// verifyHostnamePrefixCatalogJSONRoundTrip transforms named raw fuzz parameters
// into a bounded assignment, encodes it, passes the v1beta2 representation
// through JSON, and verifies exact ordered reconstruction. Time is
// O(n*m*(k+l)); space is O(n*(m+k+l*m)+p), dominated by catalog construction,
// compact encoding, and JSON reconstruction.
func verifyHostnamePrefixCatalogJSONRoundTrip(t *testing.T, parameters hostnamePrefixFuzzParameters) {
	t.Helper()
	parameterDescription := fmt.Sprintf(
		"nodes=%d groups=%d depth=%d width=%d seed=%d multiLevel=%d randomNames=%d maxLength=%d maxHyphens=%d",
		parameters.rawNodeCount,
		parameters.rawGroupCount,
		parameters.rawPrefixDepth,
		parameters.rawSegmentWidth,
		parameters.seed,
		parameters.rawMultiLevel,
		parameters.rawRandomNames,
		parameters.rawMaxHostnameLength,
		parameters.rawMaxHyphenCount,
	)

	nodeCount := int(parameters.rawNodeCount % (maxNodeCount + 1))
	groupCount := 1
	if nodeCount > 0 {
		groupCount += int(parameters.rawGroupCount) % min(nodeCount, 64)
	}
	input := fuzzHostnamePrefixAssignment(hostnamePrefixAssignmentParameters{
		nodeCount:         nodeCount,
		groupCount:        groupCount,
		prefixDepth:       int(parameters.rawPrefixDepth % 6),
		segmentWidth:      1 + int(parameters.rawSegmentWidth%8),
		seed:              parameters.seed,
		multiLevel:        parameters.rawMultiLevel&1 != 0,
		randomNames:       parameters.rawRandomNames&1 != 0,
		maxHostnameLength: 10 + int(parameters.rawMaxHostnameLength%21),
		maxHyphenCount:    int(parameters.rawMaxHyphenCount % 15),
	})
	encoded := prefixRunsOptimizedTopologyAssignmentEncoding(input)
	if got := TotalDomainCount(encoded); got != len(input.Domains) {
		t.Fatalf("%s: unexpected total domain count: got %d, want %d", parameterDescription, got, len(input.Domains))
	}
	if got := len(encoded.Slices); got > maxTopologyAssignmentSlices {
		t.Fatalf("%s: too many slices: got %d, want <= %d", parameterDescription, got, maxTopologyAssignmentSlices)
	}
	for i, slice := range encoded.Slices {
		if slice.DomainCount > maxDomainsPerTopologyAssignmentSlice {
			t.Fatalf("%s: slice %d has %d domains, want <= %d", parameterDescription, i, slice.DomainCount, maxDomainsPerTopologyAssignmentSlice)
		}
	}

	serialized, err := json.Marshal(encoded)
	if err != nil {
		t.Fatalf("%s: serialize topology assignment: %v", parameterDescription, err)
	}
	var wire kueue.TopologyAssignment
	if err := json.Unmarshal(serialized, &wire); err != nil {
		t.Fatalf("%s: deserialize topology assignment: %v", parameterDescription, err)
	}
	decoded := InternalFrom(&wire)
	if diff := cmp.Diff(input, decoded, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("%s: unexpected JSON round trip (-want,+got):\n%s", parameterDescription, diff)
	}
}

// FuzzHostnamePrefixCatalogJSONRoundTrip verifies that the catalog
// encoder preserves topology levels, domain order, values, and pod counts after
// JSON serialization and deserialization. Normal go test runs the curated seed
// corpus; go test -fuzz generates and minimizes additional inputs.
func FuzzHostnamePrefixCatalogJSONRoundTrip(f *testing.F) {
	addHostnamePrefixFuzzSeed(f, hostnamePrefixFuzzParameters{
		rawNodeCount:         0,
		rawGroupCount:        0,
		rawPrefixDepth:       0,
		rawSegmentWidth:      0,
		seed:                 0,
		rawMultiLevel:        0,
		rawRandomNames:       0,
		rawMaxHostnameLength: 0,
		rawMaxHyphenCount:    0,
	})
	addHostnamePrefixFuzzSeed(f, hostnamePrefixFuzzParameters{
		rawNodeCount:         1,
		rawGroupCount:        1,
		rawPrefixDepth:       1,
		rawSegmentWidth:      1,
		seed:                 1,
		rawMultiLevel:        0,
		rawRandomNames:       0,
		rawMaxHostnameLength: 0,
		rawMaxHyphenCount:    0,
	})
	addHostnamePrefixFuzzSeed(f, hostnamePrefixFuzzParameters{
		rawNodeCount:         32,
		rawGroupCount:        4,
		rawPrefixDepth:       5,
		rawSegmentWidth:      8,
		seed:                 2,
		rawMultiLevel:        1,
		rawRandomNames:       0,
		rawMaxHostnameLength: 0,
		rawMaxHyphenCount:    0,
	})
	// Random-name seeds cover variable 10-30 byte names with sparse and dense
	// '-' distributions in both single-level and multi-level assignments.
	addHostnamePrefixFuzzSeed(f, hostnamePrefixFuzzParameters{
		rawNodeCount:         64,
		rawGroupCount:        1,
		rawPrefixDepth:       0,
		rawSegmentWidth:      1,
		seed:                 3,
		rawMultiLevel:        0,
		rawRandomNames:       1,
		rawMaxHostnameLength: 20,
		rawMaxHyphenCount:    1,
	})
	addHostnamePrefixFuzzSeed(f, hostnamePrefixFuzzParameters{
		rawNodeCount:         128,
		rawGroupCount:        1,
		rawPrefixDepth:       0,
		rawSegmentWidth:      1,
		seed:                 4,
		rawMultiLevel:        1,
		rawRandomNames:       1,
		rawMaxHostnameLength: 20,
		rawMaxHyphenCount:    14,
	})
	// This full-scale seed exercises prefix-depth backoff after the initial
	// deepest-prefix grouping exceeds maxTopologyAssignmentSlices, then chunks
	// the selected run across the per-slice domain limit.
	addHostnamePrefixFuzzSeed(f, hostnamePrefixFuzzParameters{
		rawNodeCount:         maxNodeCount,
		rawGroupCount:        1,
		rawPrefixDepth:       2,
		rawSegmentWidth:      4,
		seed:                 0,
		rawMultiLevel:        1,
		rawRandomNames:       0,
		rawMaxHostnameLength: 0,
		rawMaxHyphenCount:    0,
	})

	f.Fuzz(func(
		t *testing.T,
		rawNodeCount uint32,
		rawGroupCount uint16,
		rawPrefixDepth uint8,
		rawSegmentWidth uint8,
		seed uint64,
		rawMultiLevel uint8,
		rawRandomNames uint8,
		rawMaxHostnameLength uint8,
		rawMaxHyphenCount uint8,
	) {
		verifyHostnamePrefixCatalogJSONRoundTrip(t, hostnamePrefixFuzzParameters{
			rawNodeCount:         rawNodeCount,
			rawGroupCount:        rawGroupCount,
			rawPrefixDepth:       rawPrefixDepth,
			rawSegmentWidth:      rawSegmentWidth,
			seed:                 seed,
			rawMultiLevel:        rawMultiLevel,
			rawRandomNames:       rawRandomNames,
			rawMaxHostnameLength: rawMaxHostnameLength,
			rawMaxHyphenCount:    rawMaxHyphenCount,
		})
	})
}

// TestHostnamePrefixCatalogRandomizedJSONRoundTrip runs a deterministic
// randomized corpus through the same property used by the fuzz target. This
// provides broad in-process coverage in environments that cannot launch Go's
// mutation-fuzzing subprocess. Most cases remain small to preserve iteration
// breadth; every hundredth case retains the full raw node-count range and can
// normalize to a large assignment.
func TestHostnamePrefixCatalogRandomizedJSONRoundTrip(t *testing.T) {
	rnd := rand.New(rand.NewPCG(0x6b75657565, 0x746173))
	for iteration := range 100 {
		rawNodeCount := rnd.Uint32()
		if iteration%100 != 0 {
			rawNodeCount %= smallRandomizedMaxNodeCount + 1
		}
		verifyHostnamePrefixCatalogJSONRoundTrip(t, hostnamePrefixFuzzParameters{
			rawNodeCount:         rawNodeCount,
			rawGroupCount:        uint16(rnd.Uint32()),
			rawPrefixDepth:       uint8(rnd.Uint32()),
			rawSegmentWidth:      uint8(rnd.Uint32()),
			seed:                 rnd.Uint64(),
			rawMultiLevel:        uint8(rnd.Uint32()),
			rawRandomNames:       uint8(rnd.Uint32()),
			rawMaxHostnameLength: uint8(rnd.Uint32()),
			rawMaxHyphenCount:    uint8(rnd.Uint32()),
		})
	}
}
