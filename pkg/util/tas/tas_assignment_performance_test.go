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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/features"
)

// maxTopologyAssignmentJSONBytes is the test budget used to verify assignment
// scalability. The encoder does not branch on serialized size.
const maxTopologyAssignmentJSONBytes = 1_500_000

// Generate n hex numbers of given length,
// ensuring they're distinct (and otherwise quasi-random).
// For the health of tests, this function behaves deterministically.
func randomHexIDs(n, length int) []string {
	chosen := map[string]bool{}
	res := make([]string, n)
	rnd := rand.NewChaCha8([32]byte{})
	bytes := make([]byte, (length+1)/2)
	for i := range n {
		for {
			var _, _ = rnd.Read(bytes)
			id := hex.EncodeToString(bytes)[:length]
			if !chosen[id] {
				chosen[id] = true
				res[i] = id
				break
			}
		}
	}
	return res
}

func fixedID(length int) string {
	// This is really whatever.
	return strings.Repeat("a", length)
}

func consecutiveIPs(n int) []string {
	res := make([]string, n)
	for i := range n {
		// Picking 100.10x.xxx.xxx (instead of traditional 10.x.xxx.xxx)
		// to make the test scenario a bit more adverse.
		res[i] = fmt.Sprintf("100.%d.%d.%d",
			100+i/(1<<16),
			i%(1<<16)/(1<<8),
			i%(1<<8),
		)
	}
	return res
}

// namingScheme generates a list of node names according to a specific recipe.
//
// The list should be "well-prefixing", in that namingScheme(n)[:m] should be
// an "equally good representative" of the scheme as namingScheme(m).
// (This convention helps test performance; it's used in "approxMaxNodesFor").
// To achieve that, specific schemes should rotate node "properties"
// (like node pool, region etc.) using "%" operator rather than in big fixed chunks.
type namingScheme interface {
	generate(nodes int) []string
}

type poolAndNodeBasedNaming struct {
	fixedPrefixAndSuffixLength int
	pools                      int
	nodeIDLength               int
	poolIDLength               int
}

func (n poolAndNodeBasedNaming) generate(nodes int) []string {
	res := make([]string, nodes)
	nodeIDs := randomHexIDs(nodes, n.nodeIDLength)
	poolIDs := randomHexIDs(n.pools, n.poolIDLength)
	for i := range nodes {
		res[i] = fmt.Sprintf("%s-%s-%s-%s",
			fixedID(n.fixedPrefixAndSuffixLength),
			poolIDs[i%n.pools],
			nodeIDs[i],
			fixedID(n.fixedPrefixAndSuffixLength),
		)
	}
	return res
}

type regionAndIPBasedNaming struct {
	fixedPrefixAndSuffixLength int
	regions                    int
	regionIDLength             int
}

func (n regionAndIPBasedNaming) generate(nodes int) []string {
	res := make([]string, nodes)
	nodeIPs := consecutiveIPs(nodes)
	regionIDs := randomHexIDs(n.regions, n.regionIDLength)
	for i := range nodes {
		res[i] = fmt.Sprintf("%s-%s-%s-%s",
			fixedID(n.fixedPrefixAndSuffixLength),
			regionIDs[i%n.regions],
			nodeIPs[i],
			fixedID(n.fixedPrefixAndSuffixLength),
		)
	}
	return res
}

type nodeBasedNaming struct {
	fixedPrefixAndSuffixLength int
	nodeIDLength               int
}

func (config nodeBasedNaming) generate(nodes int) []string {
	res := make([]string, nodes)
	nodeIDs := randomHexIDs(nodes, config.nodeIDLength)
	for i := range nodes {
		res[i] = fmt.Sprintf("%s-%s-%s",
			fixedID(config.fixedPrefixAndSuffixLength),
			nodeIDs[i],
			fixedID(config.fixedPrefixAndSuffixLength),
		)
	}
	return res
}

// randomNodeNaming generates unique, deterministic random node names whose
// total lengths are in [minLength,maxLength]. When hyphenEveryTwoLetters is
// false, each name contains one randomly positioned hyphen. When true, names
// follow the pattern "ab-cd-ef-..." except that a trailing hyphen is replaced
// by a random letter.
type randomNodeNaming struct {
	// minLength is the inclusive minimum generated hostname length.
	minLength int

	// maxLength is the inclusive maximum generated hostname length.
	maxLength int

	// hyphenEveryTwoLetters selects "ab-cd-..." instead of one random hyphen.
	hyphenEveryTwoLetters bool
}

const randomNodeAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// generate returns nodes unique names without sorting them. Expected time is
// O(nodes*maxLength), and output plus uniqueness-checking space is
// O(nodes*maxLength).
func (n randomNodeNaming) generate(nodes int) []string {
	result := make([]string, nodes)
	seen := make(map[string]struct{}, nodes)
	rnd := rand.New(rand.NewChaCha8([32]byte{1}))
	for nodeIndex := range nodes {
		for {
			length := n.minLength + rnd.IntN(n.maxLength-n.minLength+1)
			name := make([]byte, length)
			for position := range name {
				name[position] = randomNodeAlphabet[rnd.IntN(len(randomNodeAlphabet))]
			}
			if n.hyphenEveryTwoLetters {
				for position := 2; position+1 < len(name); position += 3 {
					name[position] = '-'
				}
			} else {
				name[1+rnd.IntN(len(name)-2)] = '-'
			}

			generated := string(name)
			if _, duplicate := seen[generated]; duplicate {
				continue
			}
			seen[generated] = struct{}{}
			result[nodeIndex] = generated
			break
		}
	}
	return result
}

type gkeNodePoolBasedNaming struct {
	pools int
}

func (n gkeNodePoolBasedNaming) generate(nodes int) []string {
	res := make([]string, nodes)
	for i := range nodes {
		poolID := i % n.pools
		nodeID := i / n.pools
		res[i] = fmt.Sprintf("gke-cluster-pool-%04d-%08x-%04x", poolID, poolID, nodeID)
	}
	return res
}

// awsNodeBasedNaming generates AWS EKS private-DNS-style hostnames from a
// consecutive range of IPv4-style addresses. For two nodes it produces
// ["ip-100-100-0-0.us-west-2.compute.internal",
// "ip-100-100-0-1.us-west-2.compute.internal"].
type awsNodeBasedNaming struct{}

func (awsNodeBasedNaming) generate(nodes int) []string {
	res := make([]string, nodes)
	for i := range nodes {
		res[i] = fmt.Sprintf("ip-100-%d-%d-%d.us-west-2.compute.internal",
			100+i/(1<<16),
			i%(1<<16)/(1<<8),
			i%(1<<8),
		)
	}
	return res
}

// azureNodePoolBasedNaming generates Azure AKS VMSS-style hostnames and
// rotates through node pools so the generated order is not grouped by reusable
// pool prefix. With two pools and three nodes it produces
// ["aks-nodepool0000-00000000-vmss000000",
// "aks-nodepool0001-00000001-vmss000000",
// "aks-nodepool0000-00000000-vmss000001"].
type azureNodePoolBasedNaming struct {
	pools int
}

func (n azureNodePoolBasedNaming) generate(nodes int) []string {
	res := make([]string, nodes)
	for i := range nodes {
		poolID := i % n.pools
		nodeID := i / n.pools
		res[i] = fmt.Sprintf("aks-nodepool%04d-%08x-vmss%06d", poolID, poolID, nodeID)
	}
	return res
}

func internalSinglePodsOn(nodes []string) *TopologyAssignment {
	res := &TopologyAssignment{
		Levels:  []string{corev1.LabelHostname},
		Domains: make([]TopologyDomainAssignment, len(nodes)),
	}
	for i, n := range nodes {
		res.Domains[i] = TopologyDomainAssignment{
			Values: []string{n},
			Count:  1,
		}
	}
	return res
}

func jsonBytes(v any) []byte {
	bytes, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bytes
}

func isTooLarge(ta *kueue.TopologyAssignment) bool {
	return len(jsonBytes(ta)) > maxTopologyAssignmentJSONBytes
}

func approxMaxNodesFor(tc performanceTestCase) int {
	step := 1_000 // We search with a reduced resolution, to speed up the test
	ceiling := 300_000
	nodeNames := tc.naming.generate(ceiling)
	found := sort.Search(ceiling/step, func(n int) bool {
		// Here we rely on "well-prefixing"; see the comment on "nodeNaming".
		return isTooLarge(V1Beta2From(internalSinglePodsOn(nodeNames[:n*step])))
	}) - 1
	return found * step
}

type performanceTestCase struct {
	name               string
	naming             namingScheme
	targetNodeCount    int
	sortNodeNames      bool
	skipApproxMaxNodes bool
}

func (tc performanceTestCase) generateNodeNames(nodes int) []string {
	nodeNames := tc.naming.generate(nodes)
	if tc.sortNodeNames {
		slices.Sort(nodeNames)
	}
	return nodeNames
}

var performanceTestCases = []performanceTestCase{
	{
		name: "pool-and-node-based naming (1000 node pools)",
		naming: poolAndNodeBasedNaming{
			pools:        1000, // happens in practice, at least in GKE
			nodeIDLength: 6,    // reached in AKS

			// reachable in AKS (<pool-name>-<8-char-id>-vmss, then let <pool-name> have 8 chars)
			poolIDLength: 22,

			fixedPrefixAndSuffixLength: 20,
		},
		targetNodeCount: 40_000,
	},
	{
		name: "pool-and-node-based naming (10 node pools)",
		naming: poolAndNodeBasedNaming{
			// Vendors tend to restrict "node pools" to 4k nodes or less,
			// so, as we care about 40k+ nodes, it seems 10+ pools will always exist.
			pools: 10,

			nodeIDLength:               6,
			poolIDLength:               22,
			fixedPrefixAndSuffixLength: 20,
		},
		targetNodeCount: 40_000,
	},
	{
		name: "region-and-IP-based naming (100 regions)",
		naming: regionAndIPBasedNaming{
			regions: 100, // EKS has 70, leaving room for growth

			// Reached in EKS ("ap-southeast-2") & ACK ("cn-zhangjiakou").
			// GKE reaches even 23 ("northamerica-northeast2") but luckily its naming is not region-based.
			regionIDLength: 14,

			fixedPrefixAndSuffixLength: 20,
		},
		targetNodeCount: 40_000,
	},
	{
		name: "region-and-IP-based naming (1 region)",
		naming: regionAndIPBasedNaming{
			regions:                    1,
			regionIDLength:             14,
			fixedPrefixAndSuffixLength: 20,
		},
		targetNodeCount: 40_000,
	},
	{
		name: "node-only-based naming",
		naming: nodeBasedNaming{
			nodeIDLength:               8, // reached in VKE
			fixedPrefixAndSuffixLength: 20,
		},
		targetNodeCount: 40_000,
	},
	{
		name: "GKE node-pool naming (1000 node pools)",
		naming: gkeNodePoolBasedNaming{
			pools: 1000,
		},
		targetNodeCount:    150_000,
		sortNodeNames:      true,
		skipApproxMaxNodes: true,
	},
}

// hostnamePrefixBenchmarkCases reuses the same namingScheme and
// performanceTestCase abstractions as the pre-existing scalability benchmark.
// Each case uses 100,000 domains, the maximum accepted by one topology
// assignment slice, so encoder comparisons process identical input sizes.
var hostnamePrefixBenchmarkCases = []performanceTestCase{
	{
		name:            "AWS",
		naming:          awsNodeBasedNaming{},
		targetNodeCount: 60_000,
	},
	{
		name:            "GKE",
		naming:          gkeNodePoolBasedNaming{pools: 1000},
		targetNodeCount: 60_000,
	},
	{
		name:            "Azure",
		naming:          azureNodePoolBasedNaming{pools: 1000},
		targetNodeCount: 60_000,
	},
	{
		name: "UUID",
		naming: nodeBasedNaming{
			nodeIDLength:               8,
			fixedPrefixAndSuffixLength: 20,
		},
		targetNodeCount: 60_000,
	},
}

// TestRandomNodeNaming verifies the benchmark generator's length, uniqueness,
// and delimiter contracts. For example, the dense pattern transforms random
// letters into names shaped like "ab-cd-ef-g", while the sparse pattern keeps
// exactly one non-terminal '-'.
func TestRandomNodeNaming(t *testing.T) {
	tests := map[string]randomNodeNaming{
		"single hyphen": {
			minLength: 10,
			maxLength: 30,
		},
		"hyphen after every two letters": {
			minLength:             10,
			maxLength:             30,
			hyphenEveryTwoLetters: true,
		},
	}
	for name, naming := range tests {
		t.Run(name, func(t *testing.T) {
			hostnames := naming.generate(1_000)
			seen := make(map[string]struct{}, len(hostnames))
			lengths := make(map[int]struct{})
			for _, hostname := range hostnames {
				if len(hostname) < naming.minLength || len(hostname) > naming.maxLength {
					t.Errorf("hostname %q has length %d, want [%d,%d]", hostname, len(hostname), naming.minLength, naming.maxLength)
				}
				if _, duplicate := seen[hostname]; duplicate {
					t.Errorf("duplicate hostname %q", hostname)
				}
				seen[hostname] = struct{}{}
				lengths[len(hostname)] = struct{}{}

				if !naming.hyphenEveryTwoLetters {
					if got := strings.Count(hostname, "-"); got != 1 {
						t.Errorf("hostname %q has %d hyphens, want 1", hostname, got)
					}
					continue
				}
				for position, character := range hostname {
					wantHyphen := position%3 == 2 && position+1 < len(hostname)
					if gotHyphen := character == '-'; gotHyphen != wantHyphen {
						t.Errorf("hostname %q at position %d: got hyphen=%t, want %t", hostname, position, gotHyphen, wantHyphen)
					}
				}
			}
			if len(lengths) == 1 {
				t.Errorf("generated hostnames use only one length: %v", lengths)
			}
		})
	}
}

func TestByteSizeLimit(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.TASAssignmentsEncodingByHostnamePrefix, true)
	for _, tc := range performanceTestCases {
		t.Run(tc.name, func(t *testing.T) {
			ta := V1Beta2From(internalSinglePodsOn(tc.generateNodeNames(tc.targetNodeCount)))
			assertTopologyAssignmentSize(t, ta, tc.targetNodeCount)

			if tc.skipApproxMaxNodes {
				return
			}
			nodesLimit := approxMaxNodesFor(tc)
			if nodesLimit < tc.targetNodeCount {
				t.Errorf("Nodes limit for naming %q is too low: got approx. %d, want >= %d", tc.name, nodesLimit, tc.targetNodeCount)
			} else {
				t.Logf("Nodes limit for naming %q is approx. %d", tc.name, nodesLimit)
			}
		})
	}
}

func assertTopologyAssignmentSize(t *testing.T, ta *kueue.TopologyAssignment, wantDomains int) {
	t.Helper()
	if got := len(ta.Slices); got > maxTopologyAssignmentSlices {
		t.Errorf("unexpected slice count: got %d, want <= %d", got, maxTopologyAssignmentSlices)
	}
	for i, slice := range ta.Slices {
		if slice.DomainCount > maxDomainsPerTopologyAssignmentSlice {
			t.Errorf("slice %d has too many domains: got %d, want <= %d", i, slice.DomainCount, maxDomainsPerTopologyAssignmentSlice)
		}
	}
	if got := TotalDomainCount(ta); got != wantDomains {
		t.Errorf("unexpected total domain count: got %d, want %d", got, wantDomains)
	}
	if bytes := len(jsonBytes(ta)); bytes > maxTopologyAssignmentJSONBytes {
		t.Errorf("topology assignment is too large: got %d bytes, want <= %d", bytes, maxTopologyAssignmentJSONBytes)
	}
}

func BenchmarkV1Beta2From(b *testing.B) {
	features.SetFeatureGateDuringTest(b, features.TASAssignmentsEncodingByHostnamePrefix, true)
	for _, tc := range performanceTestCases {
		nodeNames := tc.naming.generate(tc.targetNodeCount)

		// Sorted node names match scheduler-created hostname-only assignments for
		// prefix-runs encoding.
		// (This is because assignments are sorted by level values before encoding;
		// for multi-level assignments, hostnames are only sorted within higher levels).
		slices.Sort(nodeNames)
		ta := internalSinglePodsOn(nodeNames)

		desc := fmt.Sprintf("Naming scheme %q, %d nodes", tc.name, tc.targetNodeCount)
		b.Run(desc, func(b *testing.B) {
			for b.Loop() {
				var _ = V1Beta2From(ta)
			}
		})
	}
}

// topologyAssignmentEncoders keeps all end-to-end comparison benchmarks on the
// same func(*TopologyAssignment) *kueue.TopologyAssignment interface.
var topologyAssignmentEncoders = []struct {
	name   string
	encode func(*TopologyAssignment) *kueue.TopologyAssignment
}{
	{
		name:   "single-domain",
		encode: singleCompactTopologyAssignmentEncoding,
	},
	{
		name:   "prefix-runs-first",
		encode: compactTopologyAssignmentEncodingWithHostnamePrefixRuns,
	},
	{
		name:   "prefix-runs-optimized",
		encode: prefixRunsOptimizedTopologyAssignmentEncoding,
	},
}

// benchmarkTopologyAssignmentEncoders runs all end-to-end encoders for one
// immutable input. Input generation and optional sorting remain outside the
// timed loop.
func benchmarkTopologyAssignmentEncoders(b *testing.B, description string, ta *TopologyAssignment) {
	for _, encoder := range topologyAssignmentEncoders {
		b.Run(description+"/"+encoder.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = encoder.encode(ta)
			}
		})
	}
}

// BenchmarkCompactTopologyAssignmentEncoding compares single-domain,
// prefix-runs-first, and prefix-runs-optimized through their common end-to-end
// interface. The optimized case includes catalog construction directly from
// domains, run selection, and compact encoding in the timed operation.
func BenchmarkCompactTopologyAssignmentEncoding(b *testing.B) {
	for _, tc := range hostnamePrefixBenchmarkCases {
		for _, sorted := range []bool{false, true} {
			hostnames := tc.naming.generate(tc.targetNodeCount)
			if sorted {
				slices.Sort(hostnames)
			}
			ta := internalSinglePodsOn(hostnames)
			description := fmt.Sprintf("%s/%d/sorted=%t", tc.name, tc.targetNodeCount, sorted)
			benchmarkTopologyAssignmentEncoders(b, description, ta)
		}
	}
}

// BenchmarkCompactTopologyAssignmentEncodingRandomNodeNames compares the same
// three end-to-end encoders for 100,000 deterministic random node names in
// generated, unsorted order. Total hostname lengths vary uniformly from 10 to
// 30 bytes. The single-hyphen case has exactly one candidate per hostname; the
// every-two-letters case uses the "ab-cd-ef-..." pattern.
func BenchmarkCompactTopologyAssignmentEncodingRandomNodeNames(b *testing.B) {
	const nodes = 100_000
	testCases := []struct {
		name   string
		naming randomNodeNaming
	}{
		{
			name: "single-hyphen",
			naming: randomNodeNaming{
				minLength: 10,
				maxLength: 30,
			},
		},
		{
			name: "hyphen-after-every-two-letters",
			naming: randomNodeNaming{
				minLength:             10,
				maxLength:             30,
				hyphenEveryTwoLetters: true,
			},
		},
	}
	for _, tc := range testCases {
		hostnames := tc.naming.generate(nodes)
		ta := internalSinglePodsOn(hostnames)
		description := fmt.Sprintf("%s/%d/length=10-30/sorted=false", tc.name, nodes)
		benchmarkTopologyAssignmentEncoders(b, description, ta)
	}
}
