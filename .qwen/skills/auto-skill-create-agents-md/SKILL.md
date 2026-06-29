---
name: create-agents-md
description: How to thoroughly learn a project and author an accurate AGENTS.md by validating documentation claims against the actual code (CI, Makefile, source) rather than trusting prose alone.
source: auto-skill
extracted_at: '2026-06-29T07:28:47.293Z'
---

# Create an accurate AGENTS.md

Create a project `AGENTS.md` (contributor/agent guidance) by learning the project properly — and **validating the docs against the code**, not just summarizing the README.

## When to use

The user asks to "learn the project," "validate the readme," or "create AGENTS.md / AGENTS file / contributing guide."

## Procedure

### 1. Read all top-level docs in parallel
Batch-read everything at the repo root: `README*`, `ARCHITECTURE*`, `USAGE*`, `TESTING*`, `CONTRIBUTING*`, `Makefile`/`Justfile`, `go.mod`/`package.json`/`Cargo.toml`, `.gitignore`, `LICENSE`. This is the stated intent of the project — but treat it as *claims to verify*, not ground truth.

### 2. Extract the *enforced* invariants from the build system
Read CI workflows (`.github/workflows/*.yml`, `.gitlab-ci.yml`) and the Makefile. These encode the **real** gates that prose may misstate or omit:
- The exact commands CI runs (e.g. `gofmt -l .`, `go vet`, `go build`, `go test`).
- Env invariants (e.g. `CGO_ENABLED: '0'` set in both the Makefile *and* CI — a hard rule).
- Language/toolchain version (read `go-version-file`, `engines`, `.tool-versions`).
Cross-reference: if the Makefile `export`s a flag and CI repeats it, that's a hard invariant worth elevating in the AGENTS.md.

### 3. Explore the directory tree, then read entry points + a few internals
`list_directory` on `cmd/`, `internal/`, `src/`, etc. Then read the main entry points (`cmd/*/main.go`) and 2–3 representative internal packages to derive the *actual* conventions (headers, comments, error style, concurrency patterns).

### 4. VALIDATE — cross-check docs claims against code reality (the key step)
This is what separates an accurate AGENTS.md from a paraphrased README. Specifically:
- **File-level conventions:** grep/count for license headers, copyright lines. Example: `git grep -l "SPDX-License-Identifier" -- '*.go' | wc -l` vs total `git ls-files '*.go' | wc -l`. If 89/89 files conform, that's a hard convention — state the exact rate.
- **Package/component roll-calls:** the README's prose list of modules frequently **omits real packages**. Compare the documented package list against the *actual* `internal/`/`src/` directories. Flag every package that exists in code but is missing from the docs (e.g. this project's `adminapi` and `socks`). In AGENTS.md, add a note: "trust the directory listing over those prose lists when in doubt."
- **Commands:** confirm `make` targets actually exist before documenting them; confirm flag names appear in the code (`flag.String("uplink", ...)`).
- **Platform support claims** against build-constraint filenames (`_linux.go`, `_darwin.go`, `_windows.go`).

### 5. Verify nothing broke
After writing the markdown, re-run the project's static gates (`gofmt -l .`, `go vet ./...`, or the equivalent) to confirm the doc change didn't affect anything (a markdown file shouldn't, but confirm — and if `go vet` errors on an unrelated module-cache permission issue, recognize it as environmental, not caused by your change).

## What the AGENTS.md must contain

Structure it so an agent can act without re-reading the whole repo:
1. **What the project is** (1 short para + a roles/binaries table).
2. **Hard invariants — do not break these** (CGO flags, license headers, language version, the one architectural rule that's load-bearing like "never tunnel IP over a reliable stream"). Lead with these; they're the highest-value content.
3. **Build/test/lint — the exact commands** an agent must run, copied from CI/Makefile, plus a table of `make` targets.
4. **Repository layout** with a corrected component table (fix the doc discrepancies found in step 4).
5. **Code conventions** derived from reading real files (header+package-doc style, test framework choice, build-constraint file naming, concurrency/error patterns). Point at specific exemplar files.
6. **Essential domain context** an outsider would miss (e.g. the two data paths, the migration state machine).
7. **Pitfalls** — the gotchas that bite (wrong interface flag, mutually-exclusive flags, "rebuild the embedded asset after editing the frontend," unsupported platforms).
8. **References** to the authoritative docs in-repo.

## Anti-patterns to avoid
- Don't just paraphrase the README into AGENTS.md — that adds no value.
- Don't invent conventions you didn't verify by reading code.
- Don't omit the validation step — it's the whole point. When you find a discrepancy, call it out explicitly in the file (and tell the user in your summary).
- Don't write commands you didn't confirm exist.
