package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestScanParsesEveryKind(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "main.tf", `provider "aws" {
  region = "us-east-1"
}

resource "aws_s3_bucket" "my_bucket" {
  bucket = "b"
}

data "aws_caller_identity" "current" {}

module "vpc" {
  source = "./vpc"
}

variable "env" {
  type = string
}

output "bucket_id" {
  value = aws_s3_bucket.my_bucket.id
}

terraform {
  required_version = ">= 1.0"
}
`)

	got, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v, want none", got.Diagnostics)
	}

	want := []Block{
		{Kind: "provider", Name: "aws", Address: "provider.aws", File: "main.tf", Line: 1},
		{Kind: "resource", Type: "aws_s3_bucket", Name: "my_bucket", Address: "aws_s3_bucket.my_bucket", File: "main.tf", Line: 5},
		{Kind: "data", Type: "aws_caller_identity", Name: "current", Address: "data.aws_caller_identity.current", File: "main.tf", Line: 9},
		{Kind: "module", Name: "vpc", Address: "module.vpc", File: "main.tf", Line: 11},
		{Kind: "variable", Name: "env", Address: "var.env", File: "main.tf", Line: 15},
		{Kind: "output", Name: "bucket_id", Address: "output.bucket_id", File: "main.tf", Line: 19},
	}
	if len(got.Blocks) != len(want) {
		t.Fatalf("blocks = %+v, want %d entries", got.Blocks, len(want))
	}
	for i, w := range want {
		if got.Blocks[i] != w {
			t.Errorf("block %d = %+v, want %+v", i, got.Blocks[i], w)
		}
	}
}

func TestScanListsFilesAndSkipsNoise(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "main.tf", "")
	write(t, dir, "a.tf", "")
	write(t, dir, "prod.tfvars", "region = \"us-east-1\"\n")
	write(t, dir, "README.md", "hello")
	write(t, dir, ".terraform/modules/mod/main.tf", `resource "aws_vpc" "hidden" {}`)
	write(t, dir, "nested/deep.tf", `resource "aws_vpc" "deep" {}`)

	got, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{"a.tf", "main.tf", "prod.tfvars"}
	if len(got.Files) != len(want) {
		t.Fatalf("files = %v, want %v", got.Files, want)
	}
	for i, w := range want {
		if got.Files[i] != w {
			t.Errorf("files = %v, want %v", got.Files, want)
		}
	}
	if len(got.Blocks) != 0 {
		t.Errorf("blocks = %+v, want none (tfvars unparsed, .terraform and subdirs skipped)", got.Blocks)
	}
}

func TestScanBrokenFileStillScansOthers(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "broken.tf", "resource \"aws_s3_bucket\" {{{\n")
	write(t, dir, "good.tf", `resource "aws_s3_bucket" "ok" {}`)

	got, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got.Diagnostics) == 0 {
		t.Fatal("diagnostics = none, want at least one for broken.tf")
	}
	for _, d := range got.Diagnostics {
		if d.File != "broken.tf" {
			t.Errorf("diagnostic file = %q, want broken.tf", d.File)
		}
		if d.Line < 1 || d.Summary == "" {
			t.Errorf("diagnostic = %+v, want line and summary", d)
		}
	}
	found := false
	for _, b := range got.Blocks {
		if b.Address == "aws_s3_bucket.ok" && b.File == "good.tf" {
			found = true
		}
	}
	if !found {
		t.Errorf("blocks = %+v, want aws_s3_bucket.ok from good.tf", got.Blocks)
	}
}

func TestScanSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	write(t, outside, "secret.tf", `resource "aws_s3_bucket" "hidden" {}`)
	if err := os.Symlink(filepath.Join(outside, "secret.tf"), filepath.Join(dir, "link.tf")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	write(t, dir, "main.tf", `resource "aws_s3_bucket" "ok" {}`)

	got, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range got.Files {
		if f == "link.tf" {
			t.Errorf("files = %v, want no link.tf", got.Files)
		}
	}
	for _, b := range got.Blocks {
		if b.File == "link.tf" {
			t.Errorf("blocks = %+v, want no entry for link.tf", got.Blocks)
		}
	}
	for _, d := range got.Diagnostics {
		if d.File == "link.tf" {
			t.Errorf("diagnostics = %+v, want no entry for link.tf", got.Diagnostics)
		}
	}
	if len(got.Files) != 1 || got.Files[0] != "main.tf" {
		t.Errorf("files = %v, want [main.tf]", got.Files)
	}
	if len(got.Blocks) != 1 || got.Blocks[0].Address != "aws_s3_bucket.ok" {
		t.Errorf("blocks = %+v, want aws_s3_bucket.ok", got.Blocks)
	}
}

func TestScanSkipsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "big.tf", strings.Repeat("x", 1<<20+1))

	got, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	found := false
	for _, f := range got.Files {
		if f == "big.tf" {
			found = true
		}
	}
	if !found {
		t.Errorf("files = %v, want big.tf listed", got.Files)
	}
	diagFound := false
	for _, d := range got.Diagnostics {
		if d.File == "big.tf" {
			diagFound = true
			if d.Line != 1 || d.Summary != "file exceeds 1MB, skipped" {
				t.Errorf("diagnostic = %+v, want line 1 and skip summary", d)
			}
		}
	}
	if !diagFound {
		t.Errorf("diagnostics = %+v, want an entry for big.tf", got.Diagnostics)
	}
	for _, b := range got.Blocks {
		if b.File == "big.tf" {
			t.Errorf("blocks = %+v, want no entry for big.tf", got.Blocks)
		}
	}
}

func TestScanEmptyDirReturnsEmptySlices(t *testing.T) {
	got, err := Scan(t.TempDir())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got.Files == nil || got.Blocks == nil || got.Diagnostics == nil {
		t.Errorf("result = %+v, want non-nil slices", got)
	}
}
