package workspace

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

type Block struct {
	Kind    string `json:"kind"`
	Type    string `json:"type,omitempty"`
	Name    string `json:"name"`
	Address string `json:"address"`
	File    string `json:"file"`
	Line    int    `json:"line"`
}

type Diagnostic struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Summary string `json:"summary"`
}

type Result struct {
	Files       []string     `json:"files"`
	Blocks      []Block      `json:"blocks"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

func Scan(dir string) (Result, error) {
	result := Result{Files: []string{}, Blocks: []Block{}, Diagnostics: []Diagnostic{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result, err
	}

	parser := hclparse.NewParser()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !entry.Type().IsRegular() || (!strings.HasSuffix(name, ".tf") && !strings.HasSuffix(name, ".tfvars")) {
			continue
		}
		result.Files = append(result.Files, name)
		if !strings.HasSuffix(name, ".tf") {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Size() > 1<<20 {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{File: name, Line: 1, Summary: "file exceeds 1MB, skipped"})
			continue
		}

		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{File: name, Line: 1, Summary: err.Error()})
			continue
		}
		file, diags := parser.ParseHCL(src, name)
		for _, d := range diags {
			diag := Diagnostic{File: name, Line: 1, Summary: d.Summary}
			if d.Subject != nil {
				diag.Line = d.Subject.Start.Line
			}
			result.Diagnostics = append(result.Diagnostics, diag)
		}
		if file == nil {
			continue
		}
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, b := range body.Blocks {
			if block, ok := describe(b, name); ok {
				result.Blocks = append(result.Blocks, block)
			}
		}
	}
	return result, nil
}

func describe(b *hclsyntax.Block, file string) (Block, bool) {
	block := Block{Kind: b.Type, File: file, Line: b.DefRange().Start.Line}
	switch b.Type {
	case "resource", "data":
		if len(b.Labels) != 2 {
			return Block{}, false
		}
		block.Type, block.Name = b.Labels[0], b.Labels[1]
		block.Address = block.Type + "." + block.Name
		if b.Type == "data" {
			block.Address = "data." + block.Address
		}
	case "module", "variable", "output", "provider":
		if len(b.Labels) != 1 {
			return Block{}, false
		}
		block.Name = b.Labels[0]
		prefix := b.Type
		if b.Type == "variable" {
			prefix = "var"
		}
		block.Address = prefix + "." + block.Name
	default:
		return Block{}, false
	}
	return block, true
}
