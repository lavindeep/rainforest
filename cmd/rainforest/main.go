package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

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
		dir, noBrowser, err := parseOpenArgs(args[1:])
		if err != nil {
			usage()
		}
		if err := open(dir, noBrowser); err != nil {
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
  rainforest open [--no-browser] [dir]   start the local dashboard
  rainforest version                      print the version
`)
	os.Exit(1)
}

func parseOpenArgs(args []string) (string, bool, error) {
	dir := "."
	noBrowser := false
	options := true
	dirSet := false
	for _, arg := range args {
		if options && arg == "--" {
			options = false
		} else if options && arg == "--no-browser" {
			noBrowser = true
		} else if options && len(arg) > 0 && arg[0] == '-' {
			return "", false, fmt.Errorf("unknown flag: %s", arg)
		} else if dirSet {
			return "", false, fmt.Errorf("too many directories")
		} else {
			dir = arg
			dirSet = true
		}
	}
	return dir, noBrowser, nil
}

func open(dir string, noBrowser bool) error {
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	closeErr := make(chan error, 1)
	go func() {
		<-ctx.Done()
		closeErr <- s.Close()
	}()

	fmt.Printf("Rain Forest %s\nworkspace: %s\n%s\n", Version, workspace, s.URL())
	if runtime.GOOS == "darwin" && !noBrowser {
		_ = exec.Command("open", s.URL()).Run()
	}

	serveErr := s.Serve()
	stop()
	if errors.Is(serveErr, net.ErrClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, <-closeErr)
}
