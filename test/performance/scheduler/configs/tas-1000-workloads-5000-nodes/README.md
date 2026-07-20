# 1,000 workloads across 5,000 nodes

This TAS performance scenario creates 1,000 workloads, distributed evenly as
10 workloads in each of 100 ClusterQueues. Every workload has 50 pods and is
placed as 50 single-pod slices over 5,000 one-CPU nodes. Consequently, every
successful admission contains a topology assignment covering 50 hostname
domains.

Up to 100 workloads fit in the topology concurrently. The 1 second simulated
runtime lets a full batch accumulate before releasing nodes for later workloads.
Every ClusterQueue has 5,000 CPU of nominal quota, while its ten workloads
request only 500 CPU in total, so quota is not a scheduling constraint.

Run the scenario from the repository root and collect CPU and heap profiles:

```bash
SCALABILITY_TAS_GENERATOR_CONFIG="$(pwd)/test/performance/scheduler/configs/tas-1000-workloads-5000-nodes/generator.yaml" \
SCALABILITY_CPU_PROFILE=1 \
SCALABILITY_MEM_PROFILE=1 \
NO_SCALABILITY_KUEUE_LOGS=1 \
NO_SCALABILITY_SCRAPE=1 \
make run-tas-performance-scheduler
```

Open the CPU profile:

```bash
go tool pprof -http=:0 \
  ./bin/minimalkueue \
  ./artifacts/run-tas-performance-scheduler/minimalkueue.cpu.prof
```

Inspect cumulative CPU time in the terminal:

```bash
go tool pprof -top -cum \
  ./bin/minimalkueue \
  ./artifacts/run-tas-performance-scheduler/minimalkueue.cpu.prof
```

The emitted memory profile is an in-use heap snapshot taken at shutdown. Open
it with:

```bash
go tool pprof -http=:0 \
  ./bin/minimalkueue \
  ./artifacts/run-tas-performance-scheduler/minimalkueue.mem.prof
```

For allocation-heavy admission code, inspect cumulative allocated bytes rather
than only the objects retained at shutdown:

```bash
go tool pprof -http=:0 -sample_index=alloc_space \
  ./bin/minimalkueue \
  ./artifacts/run-tas-performance-scheduler/minimalkueue.mem.prof
```