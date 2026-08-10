package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/lavindeep/rainforest/internal/server"
)

var Version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
	}
	switch args[0] {
	case "open":
		dir := "."
		if len(args) > 1 {
			dir = args[1]
		}
		if err := open(dir); err != nil {
			fmt.Fprintln(os.Stderr, "rainforest:", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println(Version)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  rainforest open [dir]   start the local dashboard
  rainforest version      print the version
`)
	os.Exit(1)
}

func open(dir string) error {
	workspace, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", workspace)
	}

	s, err := server.New(workspace, Version)
	if err != nil {
		return err
	}
	fmt.Printf("Rain Forest %s\nworkspace: %s\n%s\n", Version, workspace, s.URL())
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", s.URL()).Run()
	}
	return s.Serve()
}
