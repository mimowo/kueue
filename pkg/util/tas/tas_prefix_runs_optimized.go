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
	"slices"
	"strings"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// Complexity notation used below:
//
//	a = number of domains in one assignment
//	m = maximum relevant hostname or topology value length in bytes, with a
//	    lower bound of 1
//	k = maximum of 1 and the number of '-' delimited prefix candidates in one
//	    hostname
//	p = number of distinct prefix IDs in the catalog
//	l = maximum of 1 and the number of topology levels
//	s = maxTopologyAssignmentSlices

// hostnamePrefixID is the numeric type used for catalog prefix selection.
// Values in [0, p) identify distinct prefix strings and are safe slice indexes.
// The catalog's sentinelPrefixID equals p and represents no selected reusable
// prefix.
type hostnamePrefixID uint32

// hostnamePrefixCatalog stores the '-' delimited prefixes of every hostname in
// one assignment. Catalog row i always describes assignment domain i, so
// unsorted input remains unsorted and needs no separate position mapping.
// Prefix IDs are stable and dense in [0, p), making them safe slice indexes.
// The catalog does not copy the original hostname strings.
//
// For assignment hostnames
//
//	0: "pool-a-0"
//	1: "pool-b-0"
//	2: "pool-a-1"
//	3: "batch-x-0"
//	4: "batch-x-1"
//
// the catalog is
//
//	offsets          = [0, 2, 4, 6, 8, 10]
//	ids              = [0, 1, 0, 2, 0, 1, 3, 4, 3, 4]
//	endsByID         = [5, 7, 7, 6, 8]
//	countsByID       = [2, 2, 1, 2, 2]
//	sentinelPrefixID = 5
//	maxDepth         = 2
//
// ID 0 is "pool-", ID 1 is "pool-a-", ID 2 is "pool-b-", ID 3 is
// "batch-", and ID 4 is "batch-x-". For example, offsets[2]:offsets[3]
// selects IDs [0, 1] for domain 2, whose hostname is "pool-a-1". Counts
// saturate at 2 because run selection only distinguishes prefixes occurring
// once from prefixes occurring at least twice.
type hostnamePrefixCatalog struct {
	// offsets maps an assignment domain index to the half-open range
	// offsets[domainIndex]:offsets[domainIndex+1] in ids. The final element marks
	// the end of the last range.
	offsets []uint32

	// ids stores every prefix occurrence. Equal prefix strings have equal IDs,
	// including when their hostnames are at non-adjacent domain indexes.
	ids []hostnamePrefixID

	// endsByID maps a prefix ID to its exclusive byte end in a hostname. A
	// uint16 is sufficient for Kubernetes hostnames, whose maximum length is
	// 253 bytes. The slice has p elements and is indexed directly by prefix ID.
	endsByID []uint16

	// countsByID stores the number of assignment hostnames containing each
	// prefix, saturated at 2. The slice has p elements and is indexed directly
	// by prefix ID.
	countsByID []uint8

	// sentinelPrefixID equals p and represents no selected reusable prefix. It
	// is compared like an ID but never indexes catalog metadata.
	sentinelPrefixID hostnamePrefixID

	// maxDepth is the largest number of prefix candidates on any assignment
	// hostname.
	maxDepth int
}

// hostnamePrefixCatalogBuilder incrementally transforms hostnames into catalog
// metadata without first copying their string headers into a parallel slice.
type hostnamePrefixCatalogBuilder struct {
	// catalog is the result under construction.
	catalog *hostnamePrefixCatalog

	// prefixIDs maps exact prefix strings to prefix IDs. The map is discarded
	// after construction and therefore never becomes catalog state.
	prefixIDs map[string]hostnamePrefixID

	// domainCount is used to estimate prefix-occurrence storage from the first
	// hostname without assuming all hostnames have the same number of segments.
	domainCount int
}

// newHostnamePrefixCatalogBuilder creates an empty builder for domainCount
// hostnames using prefixMapCapacity as a map allocation hint. Time and initial
// auxiliary space are O(domainCount+prefixMapCapacity).
func newHostnamePrefixCatalogBuilder(domainCount, prefixMapCapacity int) *hostnamePrefixCatalogBuilder {
	return &hostnamePrefixCatalogBuilder{
		catalog: &hostnamePrefixCatalog{
			offsets:    make([]uint32, domainCount+1),
			endsByID:   make([]uint16, 0, prefixMapCapacity),
			countsByID: make([]uint8, 0, prefixMapCapacity),
		},
		prefixIDs:   make(map[string]hostnamePrefixID, prefixMapCapacity),
		domainCount: domainCount,
	}
}

// prefixCapacityEstimate counts '-' delimiters to estimate prefix occurrences
// from the first hostname. "pool-a-0" produces 2 and "node" produces 0. A
// terminal '-' contributes one to this capacity hint even though addHostname
// does not treat it as a prefix candidate. Time is O(m), and auxiliary space is
// O(1).
func prefixCapacityEstimate(hostname string) int {
	return strings.Count(hostname, "-")
}

// addHostname adds all '-' delimited prefix candidates for one assignment
// domain. Given "pool-a-0" at domain index 0, it appends IDs for
// "pool-" and "pool-a-" and sets offsets[1]=2. Equal full prefixes receive the
// same ID even when their hostnames are non-adjacent. Expected time is O(m*k),
// and amortized additional space is O(k).
func (b *hostnamePrefixCatalogBuilder) addHostname(domainIndex int, hostname string) {
	if domainIndex == 0 {
		// Provider hostnames normally have a stable segment count. Cap the sample
		// at eight so one unusual first hostname cannot cause excessive retention.
		prefixesPerHostname := min(prefixCapacityEstimate(hostname), 8)
		b.catalog.ids = make([]hostnamePrefixID, 0, b.domainCount*prefixesPerHostname)
	}

	prefixIDsStart := len(b.catalog.ids)
	for prefixEnd := 1; prefixEnd < len(hostname); prefixEnd++ {
		if hostname[prefixEnd-1] != '-' {
			continue
		}
		if prefixEnd > int(^uint16(0)) {
			panic("hostname prefix does not fit uint16")
		}
		prefix := hostname[:prefixEnd]
		prefixID, found := b.prefixIDs[prefix]
		if !found {
			prefixID = hostnamePrefixID(len(b.catalog.endsByID))
			b.prefixIDs[prefix] = prefixID
			b.catalog.endsByID = append(b.catalog.endsByID, uint16(prefixEnd))
			b.catalog.countsByID = append(b.catalog.countsByID, 0)
		}
		b.catalog.ids = append(b.catalog.ids, prefixID)
		if b.catalog.countsByID[prefixID] < 2 {
			b.catalog.countsByID[prefixID]++
		}
	}
	prefixIDsEnd := len(b.catalog.ids)
	b.catalog.offsets[domainIndex+1] = uint32(prefixIDsEnd)
	b.catalog.maxDepth = max(b.catalog.maxDepth, prefixIDsEnd-prefixIDsStart)
}

// newHostnamePrefixCatalog builds a catalog directly from the
// lowest-level value of every domain, preserving assignment order. For domains
// whose hostnames are ["pool-b-0", "pool-a-0"], catalog rows 0 and 1 describe
// those hostnames respectively. Equal prefix strings receive the same ID even
// when their domains are non-adjacent. The temporary string map used to assign
// IDs is discarded after preprocessing. Once all IDs are known,
// sentinelPrefixID is set to p, immediately beyond the valid ID range.
//
// Expected time is O(a*m*k): scanning costs O(a*m), and O(a*k) string-map
// lookups may each hash O(m) bytes. Auxiliary space is O(a*k+p): O(a) offsets,
// O(a*k) prefix occurrences, and O(p) catalog metadata and temporary map
// entries.
func newHostnamePrefixCatalog(ta *TopologyAssignment) *hostnamePrefixCatalog {
	hostnameLevel := len(ta.Levels) - 1
	builder := newHostnamePrefixCatalogBuilder(len(ta.Domains), len(ta.Domains))
	for domainIndex, domain := range ta.Domains {
		builder.addHostname(domainIndex, domain.Values[hostnameLevel])
	}
	builder.catalog.sentinelPrefixID = hostnamePrefixID(len(builder.catalog.endsByID))
	return builder.catalog
}

// prefixIDsAt transforms an assignment domain index into a zero-allocation
// view of its prefix IDs. With the catalog example above, domain 2 produces
// [0, 1]. Time and auxiliary space complexity are O(1).
func (c *hostnamePrefixCatalog) prefixIDsAt(domainIndex int) []hostnamePrefixID {
	start, end := c.offsets[domainIndex], c.offsets[domainIndex+1]
	return c.ids[start:end]
}

// reusablePrefixIDAt returns the deepest reusable prefix ID at or below
// prefixDepth for one assignment domain. If none exists, it returns
// sentinelPrefixID, whose value is p. With the catalog example above,
// (domainIndex=2, prefixDepth=2) returns 1 for "pool-a-". Domain 1 instead
// returns 0 for "pool-", because "pool-b-" occurs only once. Time complexity is
// O(k) and auxiliary space is O(1).
func (c *hostnamePrefixCatalog) reusablePrefixIDAt(
	domainIndex, prefixDepth int,
) hostnamePrefixID {
	prefixIDs := c.prefixIDsAt(domainIndex)
	for candidate := min(len(prefixIDs), prefixDepth) - 1; candidate >= 0; candidate-- {
		prefixID := prefixIDs[candidate]
		if c.countsByID[prefixID] >= 2 {
			return prefixID
		}
	}
	return c.sentinelPrefixID
}

// hostnamePrefixRun is a contiguous domain run.
type hostnamePrefixRun []TopologyDomainAssignment

// runsAtDepth groups non-empty domains by contiguous reusable-prefix selections
// at one candidate depth. Adjacent domains with no reusable prefix form a run
// together. It reports fits=false as soon as chunking completed runs must
// exceed maxTopologyAssignmentSlices. For selections [a, a, b, a], it
// transforms domains [d0, d1, d2, d3] into [d0:d2], [d2:d3], and [d3:d4].
// Time complexity is O(a*k); returned storage is O(min(a,s)).
func (c *hostnamePrefixCatalog) runsAtDepth(
	domains []TopologyDomainAssignment,
	prefixDepth int,
	scratch []hostnamePrefixRun,
) ([]hostnamePrefixRun, bool) {
	runs := scratch[:0]
	runStart := 0
	previousPrefixID := c.reusablePrefixIDAt(0, prefixDepth)
	sliceCount := 0

	for domainIndex := 1; domainIndex < len(domains); domainIndex++ {
		prefixID := c.reusablePrefixIDAt(domainIndex, prefixDepth)
		if prefixID == previousPrefixID {
			continue
		}

		sliceCount += chunkCount(domainIndex-runStart, maxDomainsPerTopologyAssignmentSlice)
		if sliceCount > maxTopologyAssignmentSlices {
			return runs, false
		}
		runs = append(runs, hostnamePrefixRun(domains[runStart:domainIndex]))
		runStart = domainIndex
		previousPrefixID = prefixID
	}

	sliceCount += chunkCount(len(domains)-runStart, maxDomainsPerTopologyAssignmentSlice)
	if sliceCount > maxTopologyAssignmentSlices {
		return runs, false
	}
	return append(runs, hostnamePrefixRun(domains[runStart:])), true
}

// selectRuns transforms domains into contiguous reusable-prefix runs and retains
// the catalog prefix length selected for each run. Given assignment hostnames
// ["pool-b-0", "pool-b-1", "pool-a-0", "pool-a-1"], it produces two
// runs: pool-b followed by pool-a, both with knownHostnamePrefixEnd=7.
//
// Worst-case time complexity is O(a*k^2); returned storage is O(min(a,s)).
func (c *hostnamePrefixCatalog) selectRuns(
	domains []TopologyDomainAssignment,
) []hostnamePrefixRun {
	if len(domains) == 0 {
		return nil
	}
	if c.maxDepth == 0 {
		return []hostnamePrefixRun{domains}
	}

	runs := make([]hostnamePrefixRun, 0, min(len(domains), maxTopologyAssignmentSlices+1))
	for prefixDepth := c.maxDepth; prefixDepth >= 1; prefixDepth-- {
		var fits bool
		runs, fits = c.runsAtDepth(domains, prefixDepth, runs)
		if fits {
			return runs
		}
	}
	return []hostnamePrefixRun{domains}
}

// runsForAssignment decides whether catalog run selection applies. A
// multi-domain assignment whose lowest level is kubernetes.io/hostname uses
// selectRuns; every other assignment becomes one unhinted run. For assignment
// hostnames ["pool-a-1", "pool-a-0"], it returns one run with
// knownHostnamePrefixEnd=7.
//
// Worst-case time complexity is O(a*k^2); returned storage is O(min(a,s)).
func (c *hostnamePrefixCatalog) runsForAssignment(ta *TopologyAssignment) []hostnamePrefixRun {
	if len(ta.Domains) <= 1 || len(ta.Levels) == 0 || !IsLowestLevelHostname(ta.Levels) {
		return []hostnamePrefixRun{ta.Domains}
	}
	return c.selectRuns(ta.Domains)
}

// appendCompactSlicesForRun chunks one selected run and preserves its proven
// hostname-prefix end on every resulting compact slice. A run of 100,001
// domains becomes slices of 100,000 and 1 domains, both carrying the same hint
// into the shared compact encoder. Time complexity is O(a*l*m); auxiliary space
// is O(a) plus the appended representation.
func appendCompactSlicesForRun(
	encoded []kueue.TopologyAssignmentSlice,
	levels []string,
	run hostnamePrefixRun,
) []kueue.TopologyAssignmentSlice {
	for chunk := range slices.Chunk(run, maxDomainsPerTopologyAssignmentSlice) {
		encoded = append(encoded, compactSliceEncoding(
			levels,
			chunk,
		))
	}
	return encoded
}

// prefixRunsOptimizedTopologyAssignmentEncoding builds a prefix catalog from
// the assignment's lowest-level hostnames, selects contiguous prefix runs, and
// passes the selected prefix lengths to the shared compact-slice encoder.
// Catalog row i corresponds directly to domain i, so arbitrary input order is
// preserved and sorted input is not required.
//
// Given unsorted hostname domains ["pool-b-0", "pool-b-1", "pool-a-0",
// "pool-a-1"], run selection produces a pool-b run followed by a pool-a run
// without reordering either run or its domains. For non-hostname, empty, or
// single-domain assignments, the catalog is unnecessary and the assignment is
// encoded as one unhinted run.
//
// For hostname assignments, expected time complexity is
// O(a*(m*k+k^2+l*m)); auxiliary space complexity is O(a*k+p), plus the
// returned representation.
func prefixRunsOptimizedTopologyAssignmentEncoding(
	ta *TopologyAssignment,
) *kueue.TopologyAssignment {
	catalog := &hostnamePrefixCatalog{}
	if len(ta.Domains) > 1 && len(ta.Levels) > 0 && IsLowestLevelHostname(ta.Levels) {
		catalog = newHostnamePrefixCatalog(ta)
	}
	out := &kueue.TopologyAssignment{Levels: ta.Levels, Slices: []kueue.TopologyAssignmentSlice{}}
	for _, run := range catalog.runsForAssignment(ta) {
		out.Slices = appendCompactSlicesForRun(out.Slices, ta.Levels, run)
	}
	return out
}
