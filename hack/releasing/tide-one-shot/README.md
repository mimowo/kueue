# tide-one-shot: emergency Kueue merges

Run one selected PR through Tide's normal merge decision once. **Step 5 performs the actual merge** if the PR qualifies. A run does not wait for new CI jobs to finish.

This runbook targets Prow revision [`cd1c1dbd2`](https://github.com/kubernetes-sigs/prow/tree/cd1c1dbd2), referenced by the production deployment on 2026-09-30. `tide-one-shot` is the locally built binary below. It still needs GitHub and the real Prow Kubernetes API; it cannot work around an ongoing GitHub API outage.

## Safety

- **Same merge logic:** `--run-once` calls the controller's normal `Sync()`: query eligibility, blockers, mergeability, required contexts, and presubmit results for the PR head and observed base, under the production policy. This includes the policy's existing exceptions. The only source change below disables background status writes to unrelated PRs; the merge decision and merge API call stay unchanged.
- **Maintainer access:** use your own GitHub credentials. GitHub enforces their existing merge permissions and applicable branch rules; Tide grants no additional permissions. Tide does **not** check membership in Kueue's maintainer team or `OWNERS`. Enforce “maintainers only” through repository/target-branch permissions, with protection bypass disabled for these users. Do not use shared Prow bot/App credentials or an identity exempt from the branch rules.
- **Concurrent use:** isolated previews can run concurrently. **Live merges into the same branch must be serialized**, including production Tide and manual merges. Both processes could validate against base `B`; after one merges, the other can still merge an unchanged PR head onto the newer, untested base. GitHub's conflict check does not establish that the combined changes pass CI. A shared lock must cover the entire fresh sync through merge confirmation; neither a PR label nor `flock` on separate laptops provides that lock. This runbook uses an agreed exclusive merge window, not an implemented distributed lock.

Static analysis supporting these limits:

| Check | Source |
| --- | --- |
| One-shot invokes normal sync; leader election is explicitly absent | [`main.go`](https://github.com/kubernetes-sigs/prow/blob/cd1c1dbd2/cmd/tide/main.go#L154-L291) |
| Jobs are selected against the observed base and matching PR head | [`dividePool`](https://github.com/kubernetes-sigs/prow/blob/cd1c1dbd2/pkg/tide/tide.go#L1946-L1997), [`accumulate`](https://github.com/kubernetes-sigs/prow/blob/cd1c1dbd2/pkg/tide/tide.go#L1041-L1143) |
| Merge supplies expected **head** SHA, with no expected-base parameter or permission override | [`prepareMergeDetails` / `mergePRs`](https://github.com/kubernetes-sigs/prow/blob/cd1c1dbd2/pkg/tide/github.go#L214-L283), [`MergeDetails` / `Merge`](https://github.com/kubernetes-sigs/prow/blob/cd1c1dbd2/pkg/github/client.go#L4194-L4270) |
| GitHub authorizes the merge; branch restrictions can limit eligible actors | [Merge API](https://docs.github.com/en/rest/pulls/pulls#merge-a-pull-request), [branch protection](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches) |

## Merge method and linear history

**With Kueue's current configuration, one-shot squash-merges just like the controller. No additional CLI option is needed.** Step 3 preserves the [production setting](https://github.com/kubernetes/test-infra/blob/master/config/prow/config.yaml#L918), verified on 2026-09-30:

```yaml
tide:
  merge_method:
    kubernetes-sigs/kueue: squash
```

Each squash merge adds one new commit with one parent to the target branch, preserving linear history for these new changes. It does not rewrite existing history or remove merge commits created earlier.

Before merging, retain this setting and check for overrides. Tide [defaults to `merge` if no method applies](https://github.com/kubernetes-sigs/prow/blob/cd1c1dbd2/pkg/config/tide.go#L421-L514); more-specific branch settings and [configured PR labels](https://github.com/kubernetes-sigs/prow/blob/cd1c1dbd2/pkg/tide/github.go#L645-L681) can change the method. In the current configuration, `tide/merge-method-merge` requests a merge commit, while `tide/merge-method-rebase` requests rebase merging: linear, but potentially several commits per PR. For the normal Kueue result, use the existing squash policy without these overrides. `--run-once` controls how many syncs run, not the merge method.

The green button uses its selected dropdown method. **Create a merge commit** retains the PR commits and adds a commit with two parents, explaining a non-linear graph if that option was selected. For the same history shape as Kueue's Tide, select **Squash and merge**. See [GitHub's merge-method documentation](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/about-merge-methods-on-github).

## Prerequisites

- Bash, Git, Go 1.26.4 or newer, Python 3 with venv support, curl, `kubectl`, and authenticated `gh`.
- Your personal GitHub credentials can read PRs/checks, manage the temporary label, write commit statuses, and merge. Satisfy any organization token/SSO requirements. A successful preview does not prove write permissions.
- Prow operators provide a **read-only** kubeconfig for the real Prow cluster: `get/list/watch` ProwJobs in namespace `default`, with writes denied. An empty local cluster is insufficient.
- Coordinate with operators and other maintainers. Capture the configuration before any emergency query exclusions, and prevent other writes to the target branch during the final preview and merge. If exclusivity cannot be established, stop.

Run all commands in the same local Bash session. Replace the PR number and kubeconfig path.

## 1. Set up credentials and access

```bash
set -euo pipefail
umask 077
export TIDE_WORK="$(mktemp -d "${TMPDIR:-/tmp}/tide-one-shot.XXXXXX")"
export TIDE_REPO=kubernetes-sigs/kueue
export TIDE_PR=12345
export TIDE_KUBECONFIG=/absolute/path/to/prow-readonly.kubeconfig

gh auth status --hostname github.com
gh auth token --hostname github.com > "$TIDE_WORK/github-token"
tide_gh() {
  GH_HOST=github.com GH_TOKEN="$(cat "$TIDE_WORK/github-token")" gh "$@"
}
tide_gh api user --jq '{login, type}'  # Verify this is your personal account.
test "$(tide_gh api user --jq .type)" = User
test "$(tide_gh api "repos/$TIDE_REPO" --jq .permissions.push)" = true

kubectl --kubeconfig="$TIDE_KUBECONFIG" config current-context
for verb in get list watch; do
  test "$(kubectl --kubeconfig="$TIDE_KUBECONFIG" auth can-i \
    "$verb" prowjobs.prow.k8s.io -n default)" = yes
done
for verb in create update patch delete deletecollection; do
  test "$(kubectl --kubeconfig="$TIDE_KUBECONFIG" auth can-i \
    "$verb" prowjobs.prow.k8s.io -n default || true)" = no
done
```

The GitHub permission check is a preflight, not a maintainer-membership check. GitHub makes the authorization decision when merging.

## 2. Build the pinned binary

The background status controller queries all open PRs in the configured repository. Force **only that client** into dry-run mode so the temporary selector cannot change other PRs' `tide` statuses.

```bash
git clone --filter=blob:none https://github.com/kubernetes-sigs/prow.git "$TIDE_WORK/prow"
git -C "$TIDE_WORK/prow" checkout --detach cd1c1dbd2
python3 - <<'PY'
import os, pathlib
p = pathlib.Path(os.environ['TIDE_WORK']) / 'prow/cmd/tide/main.go'
old = 'githubStatus, err := o.github.GitHubClientWithLogFields(o.dryRun,'
new = 'githubStatus, err := o.github.GitHubClientWithLogFields(true,'
text = p.read_text()
assert text.count(old) == 1, 'Unexpected source revision; stop and review'
p.write_text(text.replace(old, new))
PY
git -C "$TIDE_WORK/prow" diff -- cmd/tide/main.go
mkdir -p "$TIDE_WORK/bin"
(cd "$TIDE_WORK/prow" && GOTOOLCHAIN=auto go build \
  -o "$TIDE_WORK/bin/tide-one-shot" ./cmd/tide)
```

## 3. Scope the production policy to one PR

Tide has no PR-number flag. Add a unique label to only the chosen PR and require it **in addition to** the existing query conditions. Keep the full effective configuration, including job definitions and context policy.

```bash
curl --fail --silent --show-error --location https://prow.k8s.io/config \
  -o "$TIDE_WORK/config.production.yaml"
tide_gh api "repos/$TIDE_REPO/pulls/$TIDE_PR" > "$TIDE_WORK/pr.json"
python3 -m venv "$TIDE_WORK/venv"
"$TIDE_WORK/venv/bin/pip" install PyYAML
export TIDE_LABEL="tide-one-shot-${TIDE_PR}-$(python3 -c 'import secrets; print(secrets.token_hex(6))')"

"$TIDE_WORK/venv/bin/python" - <<'PY'
import copy, json, os, pathlib, yaml
w = pathlib.Path(os.environ['TIDE_WORK'])
repo, label = os.environ['TIDE_REPO'], os.environ['TIDE_LABEL']
pr = json.loads((w / 'pr.json').read_text())
assert pr['state'] == 'open' and not pr['draft'], 'PR must be open and ready'
assert pr['base']['repo']['full_name'] == repo
branch = pr['base']['ref']
cfg = yaml.load((w / 'config.production.yaml').read_text(),
                Loader=getattr(yaml, 'CSafeLoader', yaml.SafeLoader))
assert cfg['prowjob_namespace'] == 'default'
assert cfg.get('presubmits', {}).get(repo), 'Missing production job definitions'
matches = [q for q in cfg['tide']['queries']
           if (repo in q.get('repos', []) or repo.split('/')[0] in q.get('orgs', []))
           and repo not in q.get('excludedRepos', [])
           and branch not in q.get('excludedBranches', [])
           and (not q.get('includedBranches') or branch in q['includedBranches'])]
assert len(matches) == 1, 'Expected one applicable query; stop and review config'
q = copy.deepcopy(matches[0])
for key in ('orgs', 'excludedRepos', 'excludedBranches'):
    q.pop(key, None)
q.update(repos=[repo], includedBranches=[branch])
q['labels'] = q.get('labels', []) + [label]
cfg['tide']['queries'] = [q]
cfg['push_gateway'] = {}  # No production metrics writes.
(w / 'config.one-pr.yaml').write_text(yaml.dump(
    cfg, Dumper=getattr(yaml, 'CSafeDumper', yaml.SafeDumper), sort_keys=False))
print(yaml.safe_dump(q, sort_keys=False))  # Review preserved labels/exclusions.
PY

tide_gh api --method POST "repos/$TIDE_REPO/labels" \
  -f name="$TIDE_LABEL" -f color=ededed
tide_gh api --method POST "repos/$TIDE_REPO/issues/$TIDE_PR/labels" \
  -f "labels[]=$TIDE_LABEL"
```

Do not copy this selector label to another PR. Setup changes labels; the next step does not merge.

## 4. Preview

```bash
TIDE_ARGS=(
  --run-once --provider=github --pprof-port=0
  "--kubeconfig=$TIDE_KUBECONFIG"
  "--config-path=$TIDE_WORK/config.one-pr.yaml"
  "--github-token-path=$TIDE_WORK/github-token"
)
tide_run() {
  env -u KUBERNETES_SERVICE_HOST -u KUBERNETES_SERVICE_PORT \
    "$TIDE_WORK/bin/tide-one-shot" "${TIDE_ARGS[@]}" "$@"
}
tide_run --dry-run=true 2>&1 | tee "$TIDE_WORK/preview.log"
```

The environment override selects local kubeconfig behavior. At this revision, **do not add `--no-in-cluster-config`**: it skips the current-context alias required by Tide's infrastructure client.

Proceed only after reviewing `controller=sync` logs: `Subpool synced.` must show `action=MERGE`, `targets` containing only your PR, the intended branch/base SHA, and no sync errors. A simulated `Merged.` message or exit code 0 is not proof of a real merge or permission to merge.

- `WAIT`, `BLOCKED`, no matching PR, or a query error: resolve the cause and preview again. New labels may take time to appear in GitHub search.
- `TRIGGER`: required tests need running. Read-only Kubernetes access intentionally prevents job creation; trigger tests through normal Prow commands, wait, and preview again. Do not relax the job policy.

The read-only kubeconfig is essential: this revision's [job-creation path](https://github.com/kubernetes-sigs/prow/blob/cd1c1dbd2/pkg/tide/tide.go#L1486-L1529) is not guarded by the CLI dry-run flag.

## 5. Merge and verify

Hold the exclusive target-branch merge window, confirm the [squash policy and absence of overrides](#merge-method-and-linear-history), repeat the preview if necessary, then run:

```bash
tide_run --dry-run=false 2>&1 | tee "$TIDE_WORK/merge.log"
tide_gh api "repos/$TIDE_REPO/pulls/$TIDE_PR" \
  --jq '{merged, merged_at, merged_by: .merged_by.login, merge_commit_sha}'
```

**The first command can merge the PR.** It performs a fresh sync; the preview is not an authorization token or a reservation. Confirm `merged: true` through GitHub. If it remains open or the command errors, inspect the logs and current PR state before retrying. If the PR changes target branch or production policy changes, regenerate the scoped configuration first.

## 6. Clean up

```bash
tide_gh api --method DELETE "repos/$TIDE_REPO/issues/$TIDE_PR/labels/$TIDE_LABEL"
rm -f "$TIDE_WORK/github-token"
```

Keep the logs, hand the branch back to the next maintainer, and tell operators when normal Tide can resume. Each subsequent PR needs a fresh preview against the new base. The unused repository label definition can be removed separately.

Validation scope: static source review, shell syntax, and configuration transformation. No compiled-binary or end-to-end merge test was performed for this runbook.
