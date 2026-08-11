# Rain Forest

A local GUI over the Terraform CLI. Open a Terraform workspace, see it visually,
review plans as diffs on a topology graph, and (eventually) edit → plan → approve →
apply from one place. Terraform stays the source of truth for everything it does;
Rain Forest wraps it, never reimplements it.

> Edit infrastructure through its consequences, not merely through its files.

Local-first: no hosted service, no accounts, no telemetry, nothing uploaded.
Personal tool for building AWS Terraform projects; portfolio-grade in idea and
direction.

## Decisions (2026-08-10)

- **Name:** Rain Forest. Binary/CLI: `rainforest`. Repo: `lavindeep/rainforest`.
- **Backend:** Go — single binary with embedded frontend, official HashiCorp HCL
  libraries, stdlib HTTP/SSE/subprocess.
- **Frontend:** React + TypeScript + Vite, Cytoscape.js for the graph,
  **CodeMirror 6** for the editor (simple text editor, deliberately not an IDE —
  Monaco rejected).
- **First slice:** plan review, read-only. Editing comes later.
- **End state:** the full loop — edit → fmt/validate → plan → review → approve →
  apply — with the saved-plan safety model.
- **Findings:** simple per-resource heuristics only, shown as yellow "this seems
  off" nudges. No multi-hop reachability engine, no intent-policy YAML (both
  deferred; keep the findings format open so they can slot in later).
- **AWS semantic scope:** core networking only — VPC, subnet, route table/route/
  association, IGW, NAT, security groups + rules, EC2/ENI. Everything else renders
  as a generic node.
- **Graph style:** minimal and clean, explicitly not Packet Tracer. Rounded,
  typography-first cards show the resource type over its name, currently as one
  two-line label at a single size; the two-tone treatment that plays the name up
  against a smaller type waits for the planned iconic-tile redesign. No vendor
  icons either way. Containment uses a neutral elevation ladder — each nesting
  level sits a clear step above the one under it; dependency edges are thin
  and gently curved. Color is reserved for diff meaning, while extra detail is
  available on hover or selection.
- **Layout:** persistent narrow navigator sidebar on the left; everything else in
  a horizontally scrolling pane strip (see Layout below).
- **CLI philosophy:** the CLI stays minimal — initial setup, opening the dashboard,
  and later configuration. Everything else happens in the GUI; the goal is running
  the whole IaC pipeline from the dashboard, leaving the terminal only for
  credentials and edge cases.
- **Distribution:** public GitHub repo (`lavindeep/rainforest`) with releases.
  The app self-updates from GitHub Releases — no Electron/DMG; the Go binary is
  the whole install. Zero-config: install → open → point at a folder.

## Layout

The left sidebar is the file/resource navigator — persistent and narrow. The rest
of the screen is a **pane strip**: panes side by side that you scroll between
horizontally, like macOS Spaces but continuous — you're not locked to one pane
and can see parts of two at once. Widths (of the main area): topology graph 50%,
findings 33%, work panel 66%. The work panel holds tabs for the editor/source
view, diffs, diagnostics, plan changes, and live Terraform output. CSS
scroll-snap on a horizontal container; no windowing library.

## Milestones

### v0.1 — Plan review (read-only)

`rainforest open [--no-browser] [dir]` starts a local server on `127.0.0.1`
(random port, per-launch session token, no wildcard CORS) and opens the browser
unless `--no-browser` is supplied.

- File/resource navigator from parsed HCL, with resource → file:line mapping.
- Preflight panel on open: Terraform binary found + version, workspace
  initialized or not, `AWS_PROFILE`/region env shown if set — each with a
  one-line fix hint. Explicit **Init** button runs `terraform init` (exact
  command shown first; never automatic). Credential validity is not
  pre-checked — a working `plan` is the proof; credential failures surface
  Terraform's own error clearly.
- Topology graph from `terraform show -json` (state) and a saved plan:
  **Current / Proposed / Diff** views. Every node — including unsupported
  resource types — gets a simple label: resource type + name, so generic
  nodes are still "known" for what they are. Generic dependency edges for
  all resources; AWS core-networking resources additionally get containment
  grouping when exactly one parent is proven: VPC ⊃ subnet/route table/IGW/
  security group and subnet ⊃ instance/ENI/NAT. Proposed is built from the
  saved plan; Diff is the union of prior and proposed resources, marking
  created/changed/replaced/destroyed nodes and opened/closed dependency edges.
  Managed resources only are displayed, and unresolved relationships are
  omitted rather than guessed.
- Run `terraform plan -input=false -out=<file>` + `terraform show -json <file>`
  from the UI, stream output via SSE.
- Simple yellow findings: SG rule open to 0.0.0.0/0, subnet routed to an IGW,
  resource destroyed/replaced by the plan, and similar single-resource checks.
- Click node/finding → jump to HCL source (read-only view).
- Redaction server-side: sensitive values stripped before anything reaches the
  browser; raw plan/state JSON never leaves the Go process. Graph payloads
  contain only resource addresses, types, names, semantic kinds, topology
  relationships, and diff states.

### v0.2 — Edit + full loop

- CodeMirror editor: open, edit, save, undo, search, HCL highlighting; source
  diff view; `terraform fmt` and `terraform validate -json` from the UI.
- Saved-plan safety model: plan identity = hash of (binary plan, source
  snapshot, workspace/backend, variables, terraform version). Any edit or
  regenerated plan marks the reviewed plan stale and disables Apply.
- Approval screen: exact reviewed plan, counts, destroys/replacements
  highlighted, stronger confirmation for destructive changes.
- `terraform apply <saved-plan>`, streamed; then re-read state, rebuild the
  Current view, report success/failure/partial. No auto-retry.

### v0.3 — Updates + distribution

- GitHub Releases with prebuilt macOS (and later Linux) binaries and checksums.
- Built-in updater: the dashboard checks the latest release and shows an
  "update available" notice; `rainforest update` (or a dashboard button)
  downloads the new binary, verifies its checksum, and swaps it in place.
- Zero-setup first run: install → open → point at a Terraform folder. No config
  files or flags required; sensible defaults for everything.

### Deferred (not MVP)

Multi-hop reachability tracing · intent-policy file (future maybe — revisit if
it fits once findings are trusted) · ALB/NLB/target-group/RDS
semantics · Azure/GCP · hosted anything · multi-user · drag-and-drop graph
editing · state surgery (`mv`/`rm`/import) · destroy button · auto-apply ·
LLM-generated changes · native SwiftUI app · unmanaged-resource discovery.

## Invariants (kept from day one — they're cheap)

- Terraform runs as an argument-safe child process; never a shell string.
- Opening a repo never *changes* anything. Read-only state reads
  (`terraform show -json`, `terraform version`) run automatically to render
  the dashboard — with a remote backend that read uses Terraform's normal
  credential chain over the network, same as running `terraform show` in a
  terminal. `init`, plan, and apply are always explicit user actions, with
  the exact command/cwd/workspace shown first.
- Never `-lock=false`. Only the exact reviewed saved plan can be applied.
- Credentials stay in Terraform's normal AWS chain (`~/.aws/`, SSO, env vars,
  set up outside Rain Forest); no login screen, no credential input, no
  account management anywhere in the dashboard.
- Temp plans/JSON are owner-only permissioned and deleted per retention rules.
- Unknown/computed values are shown as unknown, never guessed.
- Redaction stance: Terraform's human-rendered CLI output streams to the
  dashboard verbatim — Terraform itself masks values marked sensitive, and the
  stream is identical to what a terminal shows. The machine-readable plan/state
  JSON (which contains real values) never leaves the Go process; the browser
  only ever receives sanitized summaries.

## CLI

```text
rainforest open [--no-browser] [dir]  # start local dashboard
rainforest version
# later: rainforest analyze [dir] — headless sanitized JSON/Markdown report
```
