# gitdiff-to-merge-patch

Research into the safest way to turn "what this branch changed in Git" into
"the HTTP requests that bring an API into the same state", in Go.

The context: a GitLab pipeline runs Git commands to work out the diff of the
current branch, and that diff has to become API calls against a service whose
`PATCH` endpoint speaks the RFC where `null` means *delete this field*.

## TL;DR

1. **The RFC is 7396**, not 7386. RFC 7386 was the original JSON Merge Patch
   spec (April 2014); it was obsoleted six months later by
   [RFC 7396](https://www.rfc-editor.org/rfc/rfc7396) after errata in its
   pseudocode. Same title, same semantics, corrected text. The media type is
   `application/merge-patch+json`. A lot of Go libraries — including the one
   below — still say "7386" in their docs; they mean the same thing.

2. **Do not parse the diff.** The instinct to take `git diff` output and
   transform it is the expensive, fragile path. Ask Git *which paths changed*,
   then read the **full old and new document** for each one and diff them
   structurally. Unified diff text is a line-oriented rendering for humans; a
   JSON document's structure is not recoverable from it in general.

3. **The library already exists.**
   [`github.com/evanphx/json-patch/v5`](https://github.com/evanphx/json-patch)
   has `CreateMergePatch(original, target)`, which is exactly the
   "two JSON documents → one merge patch" function. It is the library
   Kubernetes' ecosystem is built on. You do not need to write the diff
   algorithm.

4. **Bulletproofing is one line of code, and it is not the diff.** Merge
   patch has a hole: it cannot set a field *to* `null`, because `null` is the
   deletion sentinel. So after generating a patch, **apply it locally to the
   base document and compare the result against the target document.** If it
   does not match, fall back to a full-document `PUT`. That check turns
   "the API silently holds different data than Git" into "this one file was
   sent as a replacement instead", which is a non-event.

5. **Nobody has published the bridge, and the products that solve this
   problem for real don't diff commit-against-commit at all.** decK, Grizzly,
   Flux and ArgoCD all reconcile *files at HEAD* against *state fetched live
   from the API*. See [Prior art](#prior-art-has-somebody-already-built-this).

6. **Use a three-way merge, not a two-way one.** This is what `kubectl apply`
   did client-side, and it is the one lesson here that changes the code: diff
   base commit, head commit **and** the live document together. Two inputs
   cannot tell "deleted in Git" apart from "added by somebody else", so any
   two-way form either ignores the server or destroys the fields it owns.
   Implemented in `ThreeWay`; see
   [Lessons from Kubernetes](#lessons-from-kubernetes-and-kustomize).

The rest of this README is why, plus a working implementation.

## The core decision: don't parse the diff

The pipeline's shape suggests a natural but wrong design:

```
git diff  →  parse hunks  →  reconstruct JSON changes  →  merge patch
```

This fails on ordinary inputs:

- A single-line (minified) JSON file has one line. Any change replaces the
  whole line, so the hunk tells you nothing about *which field* changed.
- Reformatting shifts every line without changing any value.
- A hunk boundary can split an object across a context boundary, so you
  cannot know the JSON path of a changed line without re-parsing the file
  anyway — at which point you've done the real work and thrown it away.
- YAML block scalars, anchors and comments make line-level reasoning worse
  still.

The reliable shape is:

```
git diff --name-status   →  which paths changed, and how
git cat-file blob        →  the full document at base and at head
structural JSON diff     →  merge patch
```

Both Git commands are exact and machine-readable, and the diff happens on
parsed values rather than on text. This is also *less* code than a hunk
parser, which is the pleasant part.

Two flags matter here and are easy to get wrong:

- **`-z`.** Without it, Git quotes and escapes paths containing spaces,
  quotes or non-ASCII bytes. Every hand-rolled `--name-status` parser gets
  this wrong eventually. With `-z` the output is raw bytes with NUL
  separators.
- **`base...head` (three dots), not `base..head`.** Three dots diffs against
  the *merge base*, i.e. only what this branch introduced. Two dots also
  reports commits that landed on the target branch since you forked — which
  your pipeline would faithfully translate into API calls that *undo other
  people's work*. `internal/gitdiff.TestChangesMergeBase` pins this down.

## The Go library landscape

| Library | Produces | Use it for |
|---|---|---|
| [`evanphx/json-patch/v5`](https://github.com/evanphx/json-patch) | RFC 7396 merge patch (create + apply), RFC 6902 (apply only) | **The main recommendation.** `CreateMergePatch` is the function you want; `MergePatch` and `Equal` give you the verification step for free. Broadly deployed, stable API. |
| [`wI2L/jsondiff`](https://github.com/wI2L/jsondiff) | RFC 6902 JSON Patch | The fallback when merge patch can't express a change, and the better choice generally if the server accepts RFC 6902. Has `Factorize()` (emit `move`/`copy`), `LCS()` (sane array diffs) and `Invertible()` (prepend `test` ops). |
| [`josephburnett/jd/v2`](https://github.com/josephburnett/jd) | native jd format, RFC 6902, RFC 7396 | Reads YAML natively and translates between all three formats. Its `PathOptions` (`SET`, `MULTISET`, `setkeys`, numeric `precision`) are the only off-the-shelf answer to *"this array is really an unordered set keyed by `name`"*. Also a good CLI for rendering a human-readable diff onto the merge request. |
| `k8s.io/apimachinery/.../strategicpatch` | strategic merge patch | Only if you are patching Kubernetes objects. It needs Go struct tags to know list merge keys, so it does not generalise to arbitrary documents. |

Versions verified working together as of this writing: `json-patch v5.9.11`,
`jsondiff v0.7.1`, `jd/v2 v2.5.0`.

## The four traps, and what to do about each

### 1. Merge patch cannot express an explicit `null`

This is the one that actually loses data. RFC 7396 has exactly one sentinel
and it is overloaded:

```
original: {"retries": 3, "note": "hi"}
target:   {"retries": null, "note": "hi"}

CreateMergePatch → {"retries": null}
applied          → {"note": "hi"}          ← retries is GONE, not null
```

`CreateMergePatch` produces this happily and returns no error. If your
documents ever carry a meaningful `null` — and config files do, all the time,
as "explicitly unset" versus "not configured" — you will silently drop
fields.

**Fix: verify, don't inspect.** Rather than scanning for nulls and
special-casing them, apply the patch you just generated and compare:

```go
patch, _ := jsonpatch.CreateMergePatch(original, target)
got, _   := jsonpatch.MergePatch(original, patch)
if !jsonpatch.Equal(got, target) {
    // the patch is not faithful — fall back
}
```

This is three lines, costs microseconds, and it is *strictly stronger* than
enumerating known traps: it also catches library bugs, encoding surprises and
any future edge case nobody has thought of yet. It is the single highest-value
thing in this whole design.

`internal/docsync.TestBuildPatchNullTrap` demonstrates both the corruption and
the catch.

### 2. Arrays are replaced wholesale

RFC 7396 has no element-level array semantics. Change one entry in a
200-element list and the patch contains all 200 elements:

```
original: {"tags": ["a","b","c"]}
target:   {"tags": ["a","z","c"]}
patch:    {"tags": ["a","z","c"]}
```

Consequences worth deciding on deliberately:

- **Payload size** is bounded by your largest array, not by the size of the
  change.
- **Lost updates.** If anything else can edit that array server-side, your
  patch overwrites it entirely. Git is the source of truth or it isn't —
  decide, and if it is, make the API reject writes from anywhere else.
- If you need element-level array ops, that is the argument for RFC 6902 with
  `jsondiff.LCS()`, or for `jd`'s `SET`/`setkeys` path options.

### 3. Most "changed" files did not change

Git reports a file as modified when someone reformats it, reorders keys, or
edits a YAML comment. None of those should produce an API call. Canonicalise
first — decode to a value and re-encode with sorted keys and no whitespace —
then compare. Equal documents drop out of the plan entirely.

One detail that bites in production: decode JSON with
`json.Decoder.UseNumber()`. Without it every number round-trips through
`float64`, and integer IDs above 2^53 silently change value. `9007199254740993`
becomes `9007199254740992`, and the resulting patch "fixes" a field nobody
touched.

### 4. Merge patch has no preconditions

RFC 6902 has a `test` op; RFC 7396 has nothing. A patch computed against
commit A and applied to a server that has since moved on will apply
*anyway*, cleanly and wrongly. In a pipeline this happens whenever two merge
requests merge close together and the jobs run out of order.

The cheap fix, and the reason this repo's plan carries it: Git already
computed a content hash for every version of every file. Send the base blob
ID as a precondition and let the server refuse mismatches.

```
PATCH /api/v1/eu/rates
Content-Type: application/merge-patch+json
X-Base-Version: 5d1b8993aecfac9e956f65990d5447cc3f341d81
```

The server stores the blob ID it last applied for that resource and returns
`409 Conflict` if it differs. Standard `ETag` + `If-Match` works identically
if you already have ETags; the point is that the token must exist, because
merge patch cannot carry one itself. Then make the job re-plan against the
current state and retry, rather than force it.

## Reference implementation

A working, tested implementation of the above. ~700 lines including tests.

```
internal/gitdiff/   Git range → []Change, and (rev, path) → blob content + ID
internal/docsync/   canonicalisation, patch generation with verification,
                    two-way and three-way merge, change → HTTP request planning
cmd/gitsync/        CLI that prints the plan as JSON, curl commands, or a summary
```

Two modes. By default the planner diffs the base commit against the head
commit, which needs no API access and is correct if the server is guaranteed
to sit at base. Setting `Planner.Fetch` switches it to a three-way merge
against the live document, which is what you want in practice — see
[Lessons from Kubernetes](#lessons-from-kubernetes-and-kustomize).

```
go test ./...
go run ./cmd/gitsync -dir /path/to/repo -base origin/main -head HEAD \
    -root config -prefix /api/v1 -format summary
```

Against a repository where `config/eu/rates.json` had its rate changed, a
field removed, an array extended and its keys reordered; `config/limits.yaml`
had only a comment added and keys reordered; `config/old.json` was deleted;
and `config/added.json` is new:

```
PUT    /api/v1/added        replace      12 bytes
PATCH  /api/v1/eu/rates     merge-patch  58 bytes
DELETE /api/v1/old          none          0 bytes
```

```
curl -sSf -X PATCH "$API_BASE/api/v1/eu/rates" \
  -H "X-Base-Version: 5d1b8993aecfac9e956f65990d5447cc3f341d81" \
  -H "Content-Type: application/merge-patch+json" \
  -d '{"deprecated":null,"rate":1.25,"regions":["de","fr","es"]}'
```

`limits.yaml` produced nothing, which is the point: a comment and a key
reorder are not a change. `README.md` also changed in that commit and never
became a request, because `Resolve` skips paths that aren't documents.

### How the pieces map to the traps

- **Canonicalisation** (`docsync/canon.go`) handles trap 3, and reads YAML as
  well as JSON since GitOps-style config repos are usually YAML.
- **`buildPatch`** (`docsync/plan.go`) handles trap 1: merge patch first,
  verified; on failure, one of three configurable fallbacks — full `PUT`
  (default), an RFC 6902 patch via `jsondiff` (itself verified), or a hard
  error. The error message names the offending JSON Pointer path.
- **`IncludeBaseVersion`** handles trap 4.
- Trap 2 is a semantics decision, not a code fix; it is called out in the
  tests so nobody is surprised by it later.

### Non-obvious behaviours worth knowing

**Renames are not patches.** If the filename *is* the resource identity, a
rename is a `DELETE` of the old URL plus a `PUT` of the new one — there is no
patch that moves a resource. If the identity lives *inside* the document
(`{"id": "..."}`), the rename is invisible to the API and only the content
change is sent. The planner supports both; which one you get depends on your
`Resolve` function. Both are covered by tests.

**Git's rename detection is line-based**, so it does not fire on minified
single-line documents that are renamed *and* edited in the same commit —
similarity scores 0% and Git reports delete + add. This degrades safely here
(you get `DELETE` + `PUT`, which is correct, just chattier), but it is worth
knowing before you rely on `R` statuses. Pretty-printed documents detect fine.

**Unmerged (`U`) statuses are a hard error**, not a skip. Half-syncing a
conflicted tree is worse than failing the job.

## GitLab CI wiring

```yaml
sync-plan:
  image: golang:1.24
  variables:
    GIT_DEPTH: 0            # rename detection and merge-base need real history
  script:
    - git fetch origin "$CI_MERGE_REQUEST_TARGET_BRANCH_NAME"
    - go run ./cmd/gitsync
        -base "origin/$CI_MERGE_REQUEST_TARGET_BRANCH_NAME"
        -head "$CI_COMMIT_SHA"
        -root config -prefix /api/v1
        -format summary | tee plan.txt
  artifacts:
    paths: [plan.txt]
```

`GIT_DEPTH: 0` matters — GitLab's default shallow clone has no merge base to
compute against, and rename detection needs the objects.

Keeping *plan* and *apply* as separate jobs is worth the extra stage: the plan
renders on the merge request as a reviewable artifact, and the apply job runs
on the protected branch after merge. The plan is a plain JSON document
(`-format json`), so the apply step is a loop over requests, not a re-run of
the diff logic.

## Prior art: has somebody already built this?

Short answer: **the two halves are solved, the bridge is not, and the products
that solve the whole problem deliberately solve a different one.**

### Nothing does "git diff → JSON Patch" end to end

There is no library, in Go or elsewhere, that takes a Git range and emits
patch documents. Searching for one turns up two disjoint populations:

- **JSON-document differs** — `evanphx/json-patch`, `wI2L/jsondiff`, `jd`,
  `jsondiffpatch` (JS), `jsondiffpatch.net` (C#), `jiff` (JS), and a pile of
  browser-based generators. All take *two JSON documents*. None knows about
  Git.
- **Unified-diff parsers** — [`sourcegraph/go-diff`](https://github.com/sourcegraph/go-diff),
  [`bluekeyes/go-gitdiff`](https://github.com/bluekeyes/go-gitdiff),
  `waigani/diffparser`, `codepawfect/git-diff-parser`. All take *diff text*
  and return hunks, line ranges and file modes. They exist for code review
  tooling, lint-on-changed-lines and patch application. None reconstructs
  JSON structure, because — as above — it isn't recoverable from hunks.

So the intuition that "somebody was supposed to do that" is half-right:
people did build Git diff parsers, just not for this. The bridge doesn't
exist as a package because, once you stop trying to parse diff text, it's
about fifty lines of glue — which is most of `internal/docsync` here.

### The closest existing thing: `jd` as a Git diff driver

[`jd`](https://github.com/josephburnett/jd) is the one tool that spans both
worlds. It registers as a Git diff driver:

```bash
git config diff.jd.command 'jd --git-diff-driver'
echo "*.json diff=jd" >> .gitattributes
```

after which `git diff` on JSON files shows structural diffs instead of line
noise. That's worth doing regardless of this project — it makes config
changes readable on a merge request.

It also has the output format you need (`-f merge` for RFC 7386/7396,
`-f patch` for RFC 6902) and format translation (`-t patch2merge`). Which
means there is a **zero-code version of this entire pipeline**, worth knowing
before writing Go:

```bash
git diff --name-only "$BASE...$HEAD" -- config/ | while read -r f; do
  jd -f merge \
     <(git show "$BASE:$f") \
     <(git show "$HEAD:$f")
done
```

`jd` reads YAML natively too, so this covers the YAML case for free.

The caveats are real but bounded: the diff-driver path emits the
human-readable `jd` format rather than merge patch (the driver protocol hands
you temp file paths, so for a pipeline you'd invoke `jd` directly as above
rather than route through Git's diff machinery), and it has no verification
step, so the `null` trap is fully live. You can close that in shell —
apply the patch back with `jd -p` and compare — at which point you have
reimplemented this repo in Bash. **If your documents provably never contain
`null`, the shell version is the honest recommendation and you should not
write Go for this.** The Go implementation earns its place when you want the
verification, the no-op filtering, and the plan-then-apply split.

### The products that solve the whole problem chose a different architecture

This is the finding worth acting on. Git-to-API sync is a well-trodden
problem — [Kong's decK](https://github.com/Kong/deck), Grafana's Grizzly,
[Flux and ArgoCD](https://argo-cd.readthedocs.io/en/stable/user-guide/diff-strategies/)
all do exactly it, and decK is Go and has a documented GitHub Actions
workflow of "`deck diff` on the PR, `deck sync` on merge" that is precisely
the pipeline shape described here.

**None of them diffs commit against commit.** Every one of them diffs
*desired state* (the files at `HEAD`) against *actual state* (fetched live
from the API). The Git history is used to decide *when* to reconcile, never
*what* to send.

That distinction is not stylistic, and it maps directly onto the safety
requirement:

| | commit-to-commit (what the task implies) | desired-vs-actual (what the products do) |
|---|---|---|
| Assumes server is at `base` | yes, blindly | no |
| Job retried, or run out of order | applies twice / applies to wrong base | converges, idempotent |
| Job failed halfway through | permanently inconsistent | fixed by the next run |
| Somebody edited via the UI | drift is invisible and permanent | detected as drift |
| Needs read access to the API | no | yes, a `GET` per resource |
| Blast radius | bounded by the merge request | whatever the reconciler sees |

A commit-to-commit patch is a *blind write*: it is only correct if the server
is in exactly the state the base commit describes, and nothing in the design
checks that. Everything that can desynchronise a pipeline — a skipped job, a
retry, two merge requests merging minutes apart, a manual fix — makes it
wrong silently.

**Recommended hybrid.** Keep the Git diff, but demote what it's used for:

1. Use `git diff --name-status` to decide **which resources to touch**. This
   preserves the property the Git approach is actually good at — the blast
   radius stays bounded by the merge request, and you never need to enumerate
   every resource the API holds.
2. For each one, `GET` the current document and reconcile against **that**
   rather than against the base blob.

> **Correction.** An earlier draft of this section said step 2 should diff
> live against head — a two-way diff with a fetched left-hand side. That is
> wrong, and wrong in a data-losing direction: it cannot distinguish "removed
> in Git" from "added by somebody else", so it deletes every field the server
> owns. Step 2 needs *three* inputs, not two. See
> [Lessons from Kubernetes](#2-three-way-merge-not-two-way--and-this-corrects-my-earlier-advice),
> which is where that mistake surfaced and where the corrected algorithm is
> implemented.

`X-Base-Version` (trap 4 above) is the cheap approximation for when the API
has no readable `GET`; if it does have one, prefer the three-way merge.

There is a nice parallel in ArgoCD's move to **server-side diff**: rather than
predicting what the server will store, it does a dry-run apply and compares
the *result*. Same instinct as the local verification step in `buildPatch` —
don't reason about what a patch will do, apply it and look.

## Lessons from Kubernetes and kustomize

Kubernetes is the largest deployment of this exact problem — reconcile
declarative documents in Git against a REST API — and it has been through two
complete architectural generations. Both generations are instructive, and the
*transition between them* is the single most useful thing in this document.

### 1. Kubernetes had this architecture and abandoned it

Client-side `kubectl apply` stored the previously-applied document in an
annotation, `kubectl.kubernetes.io/last-applied-configuration`, and did a
**three-way merge** against it. That annotation plays exactly the role your
base commit plays: "what I sent last time."

They moved to Server-Side Apply because the client-held copy of "last applied"
was the weak link — it could drift from what the server actually had, it was
subject to size limits, and it could not represent more than one writer. A
base commit has the first and third problem and not the second.

The transition is worth reading as a warning about the direction of travel,
but the *client-side* design is the one that maps onto a Git pipeline, and
its central idea is the thing to steal.

### 2. Three-way merge, not two-way — and this corrects my earlier advice

`kubectl apply` never diffed two versions of a manifest. It diffed three
documents:

| kubectl | this pipeline |
|---|---|
| `last-applied-configuration` annotation | the document at the merge-base commit |
| the manifest being applied | the document at `HEAD` |
| the live object | the document fetched from the API |

Three inputs are needed because **two cannot distinguish the two ways a field
can be missing**:

- present in base, absent from head → *deleted in Git*, must be deleted on the
  server
- present in live, absent from base and head → *added by somebody else*, must
  be left alone

Diffing base against head ignores the server and blindly assumes it sits at
base. But diffing live against head — the "hybrid" recommended in the Prior
art section above — is *worse*: it cannot tell those two cases apart, so it
deletes every field the server owns. Only the three-way form is correct, and
the hybrid as originally stated would have quietly destroyed server-owned
fields. `ThreeWay` in `internal/docsync/threeway.go` implements it:

```go
patch, want, err := docsync.ThreeWay(base, head, live)
```

It returns the patch *and* the document that applying it should produce —
which is head plus the fields the server owns, not head alone. Verifying
against head alone would reject every correct patch the moment the server
held one extra field.

Set `Planner.Fetch` to switch the planner onto this path. The properties it
buys, each pinned by a test in `threeway_test.go`:

| scenario | two-way (base→head) | three-way |
|---|---|---|
| job re-run after success | patches again | empty patch |
| job failed halfway | stays inconsistent | repaired next run |
| field added via the UI | invisible | preserved |
| field removed in Git | deleted | deleted |
| server drifted, commits unchanged | invisible forever | corrected |

That last row is the one neither of the earlier designs could reach at all: if
nothing changed between the two commits, a commit-to-commit diff has nothing
to say, and the drift persists indefinitely.

### 3. The array problem is solved by *metadata*, not by a better algorithm

Trap 2 above — RFC 7396 replacing arrays wholesale — is the thing Strategic
Merge Patch exists to fix, and the fix is not a cleverer diff. It is
**declaring the semantics of each list out of band**:

- In Go API types, struct tags: `patchStrategy:"merge"` and
  `patchMergeKey:"name"` — merge this list by matching elements on their
  `name` field rather than replacing it.
- In CRDs and OpenAPI: `x-kubernetes-list-type: map|set|atomic` with
  `x-kubernetes-list-map-keys`, and `x-kubernetes-map-type:
  granular|atomic`.

The general lesson is that **list merge semantics are not recoverable from the
data** — `["a","b"]` could be an ordered sequence, an unordered set, or a
keyed collection, and only a human knows which. Every system that gets this
right takes the answer as configuration. `jd`'s `PathOptions` (`SET`,
`MULTISET`, `setkeys`) are the same idea in a much smaller package, and are
the practical route here if your arrays are really keyed collections.

### 4. When one sentinel isn't enough, add vocabulary

Strategic Merge Patch also shows what to do about the `null` ambiguity if
your API surface is still negotiable. Rather than overloading one value, it
adds explicit directives: `$patch: delete`, `$patch: replace`,
`$setElementOrder/<list>`, `$deleteFromPrimitiveList/<list>`, `$retainKeys`.

The design principle is that **intent should be stated, not inferred**.
RFC 7396 infers deletion from a value that is also legal data, and that single
decision is the origin of the entire fallback machinery in this project. If
you are defining the API rather than consuming it, one explicit directive
removes the need for all of it.

### 5. "Apply" is a different verb from PUT and PATCH

The clearest statement of the idea, from `structured-merge-diff`'s docs:

> PUT/PATCH says: "Make the object look EXACTLY like X". APPLY says: "The
> fields I manage should now look exactly like this (but I don't care about
> other fields)."

Server-Side Apply tracks per-field ownership in `metadata.managedFields`, so
two writers touching disjoint fields never conflict, and two writers touching
the *same* field get an explicit error instead of last-write-wins.

For this pipeline that translates to a question worth answering before
writing any more code: **is Git the sole owner of these documents, or one
writer among several?** If sole, the three-way merge is sufficient. If not,
the honest design is to declare which fields Git owns and have the server
reject writes to fields owned by others — and at that point you are
reimplementing SSA, which is a good reason to look at whether the API can
adopt it wholesale.

### 6. Reusable Go code, if you want to go further

- **`sigs.k8s.io/kustomize/kyaml/yaml/merge3`** — a three-way merge over
  arbitrary YAML/JSON nodes, with associative-key list merging and the SMP
  directives, and it preserves comments because it works on YAML nodes rather
  than on decoded values. Not Kubernetes-specific in its core.
  `merge2` is the two-way form.
- **`sigs.k8s.io/structured-merge-diff`** — the engine behind SSA:
  schema-typed values, field sets, and ownership tracking.

Both are heavier dependencies than `evanphx/json-patch`, and both assume you
can describe your documents with a schema. Worth adopting if the array
semantics or the multi-writer problem turn out to be real for you; not worth
it just to compute a merge patch.

### On `antchfx/xpath` — a different axis

[`antchfx/xpath`](https://github.com/antchfx/xpath) is an XPath engine whose
`NodeNavigator` interface lets one query engine drive XML, HTML and JSON
(via `jsonquery`) from a single implementation. It is a **selection** library,
not a diff or patch library, so it does not compete with anything above.

It is still worth the detour, because it names the alternative to diffing
entirely. Kustomize's transformers work this way: a `FieldSpec` names a path,
a transformer rewrites whatever is there, and no diff is ever computed. If
part of your sync is rule-shaped — *"always set `region` to the CI variable"*,
*"strip every `debug` flag"* — that is a path-addressed transform, and
expressing it as a document diff is the wrong shape. Diffing answers "mirror
whatever the file says"; addressing answers "enforce this invariant." Real
pipelines usually want both.

One design detail worth copying from the patch RFCs, though: they deliberately
address nodes with **JSON Pointer** (RFC 6901), not with a query language.
Pointer resolves to exactly one location or fails. XPath and JSONPath
(standardised as RFC 9535 in 2024) can match zero, one or many nodes depending
on the document — which is exactly what you want for a query and exactly what
you do not want for a patch, where "apply this to whatever matched" is
non-deterministic against a document you have not seen. Use the query language
to *find* things; use Pointer-shaped addressing to *change* them.

## When to use something other than merge patch

Merge patch is the right default *because the API already speaks it* — the
question in this repo is how to generate one safely, and the answer is
"library plus verification". But if the API surface is still negotiable:

- **Arrays that are really sets or keyed collections** — RFC 7396 will fight
  you forever. RFC 6902 via `jsondiff` with `LCS()`, or `jd` with
  `setkeys`, express these properly.
- **Concurrency guarantees in the payload** — RFC 6902's `test` op, which
  `jsondiff.Invertible()` emits automatically, makes the patch self-checking
  without a side-channel header.
- **Documents with meaningful nulls everywhere** — if the fallback fires on
  most files, merge patch is the wrong format for that data and you are
  better off with RFC 6902 as the primary.

For everything else, merge patch's readability on a merge request diff is a
real operational advantage, and it wins.

## Sources

- [RFC 7396 — JSON Merge Patch](https://www.rfc-editor.org/rfc/rfc7396) (obsoletes RFC 7386)
- [RFC 6902 — JavaScript Object Notation (JSON) Patch](https://www.rfc-editor.org/rfc/rfc6902)
- [RFC 6901 — JSON Pointer](https://www.rfc-editor.org/rfc/rfc6901)
- [evanphx/json-patch](https://github.com/evanphx/json-patch) — [pkg.go.dev](https://pkg.go.dev/github.com/evanphx/json-patch/v5)
- [wI2L/jsondiff](https://github.com/wI2L/jsondiff) — [pkg.go.dev](https://pkg.go.dev/github.com/wI2L/jsondiff)
- [josephburnett/jd](https://github.com/josephburnett/jd) — [pkg.go.dev](https://pkg.go.dev/github.com/josephburnett/jd)
- [sourcegraph/go-diff](https://github.com/sourcegraph/go-diff) and [bluekeyes/go-gitdiff](https://github.com/bluekeyes/go-gitdiff) — unified diff parsers, i.e. the approach this project argues against
- [Kong decK](https://github.com/Kong/deck) and [its GitOps workflow](https://konghq.com/blog/engineering/gitops-for-kong-managing-kong-declaratively-with-deck-and-github-actions) — the closest existing product to this pipeline
- [Argo CD diff strategies](https://argo-cd.readthedocs.io/en/stable/user-guide/diff-strategies/) — desired-vs-actual reconciliation and server-side diff
- [Kubernetes strategic merge patch](https://github.com/kubernetes/community/blob/main/contributors/devel/sig-api-machinery/strategic-merge-patch.md) — merge keys and the `$patch` directives
- [Server-Side Apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/) and [KEP-555](https://github.com/kubernetes/enhancements/blob/master/keps/sig-api-machinery/555-server-side-apply/README.md) — field ownership, and why the last-applied annotation was retired
- [kustomize `kyaml/yaml/merge3`](https://pkg.go.dev/sigs.k8s.io/kustomize/kyaml/yaml/merge3) and [`merge2`](https://pkg.go.dev/sigs.k8s.io/kustomize/kyaml/yaml/merge2) — reusable three-way and two-way merges over YAML nodes
- [structured-merge-diff](https://github.com/kubernetes-sigs/structured-merge-diff) — the schema-aware engine behind Server-Side Apply
- [antchfx/xpath](https://github.com/antchfx/xpath) — XPath over XML/HTML/JSON; the addressing axis rather than the diffing one
- [RFC 9535 — JSONPath](https://www.rfc-editor.org/rfc/rfc9535) — the standardised JSON query language, as distinct from JSON Pointer
