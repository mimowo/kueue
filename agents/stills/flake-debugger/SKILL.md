---
name: flake-debugger
description: Expertise in debugging Kueue flakes
---

# Kueue flake debugger

You are an expert in Kueue which is the project for Workload orchestration.

## Expertise

- Deep understanding of Kueue.

## Flake debugging

### Step one - initial build-log analysis

When asked to debug a flake with the given link to Github issue like this https://github.com/kubernetes-sigs/kueue/issues/9591
then identify the prow link to the failure.

Please report the list of all prow links, for example:

```sh
curl -Lv https://github.com/kubernetes-sigs/kueue/issues/9591 2>/dev/null | grep -e "href=\"https://prow\.k8s\.io/view/gs/kubernetes-ci-logs/pr-logs/pull"
```

Then, extract the links. 

Choose the first, let call it PROW_LOG, eg:

```sh
BASE_PROW_LOG=https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_kueue/9528/pull-kueue-test-integration-baseline-release-0-15/2027424372553158656
```

Then appenend "/build-log.txt", down load using curl, say

```sh
curl -Lv ${BASE_PROW_LOG}/build-log.txt -obuild-log.txt
```
Then grep the build-log around the failing lines:

```sh
cat build-log.txt | grep -ab40 "\[FAILED\]"
```
Output the lines, summarize the failure line, the name of the failed test and the namespace name.

### Step 2 - analyze kubelet logs 

When the timeouts are exceeded it is often useful to check kubelet logs for the namespace.

To fetch kubelet logs for Kueue on a specific node for specific test suite:

${BASE_PROW_LOG}$/artifacts/{{suite}}/{{KIND_WORKER}}/kubelet.log

For example:

${BASE_PROW_LOG}/artifacts/run-test-e2e-singlecluster-1.32.8/kind-worker/kubelet.log

It is useful to check Kubelet logs from all workers, so often also `kind-worker2` etc.

Then, by grepping by the namespace you will be able to track the Pods.
