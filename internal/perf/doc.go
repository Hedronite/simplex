// Package perf holds the benchmarks, profiles and soak tests for one probe
// pass. It has no production code: every file but this one is a test, and the
// generators build synthetic documents and configs in memory from
// documentation values only. Nothing here contacts a daemon, a host or the
// network, and nothing here is compiled into the shipped binary.
package perf
