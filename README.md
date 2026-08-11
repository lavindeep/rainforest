# Rain Forest

A local dashboard for Terraform/AWS IaC — visualize, plan, review, and apply from
one GUI.

**Status:** v0.1 scaffold — under active development.

## Quick start

```sh
make build
./rainforest open path/to/terraform
```

The binary starts a server on `127.0.0.1` (random port, per-launch token) and
opens your browser.

## Stack

- Go — single binary with the frontend embedded
- React + TypeScript + Vite
- Native inline SVG (graph); CodeMirror 6 (editor) is planned

## Privacy

Local-first: no hosted service, no accounts, no telemetry. Nothing leaves your
machine.
