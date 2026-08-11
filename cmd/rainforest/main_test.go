package main

import "testing"

func TestParseOpenArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		dir       string
		noBrowser bool
		wantErr   bool
	}{
		{name: "no args", dir: "."},
		{name: "dir only", args: []string{"workspace"}, dir: "workspace"},
		{name: "flag only", args: []string{"--no-browser"}, dir: ".", noBrowser: true},
		{name: "flag then dir", args: []string{"--no-browser", "workspace"}, dir: "workspace", noBrowser: true},
		{name: "dir then flag", args: []string{"workspace", "--no-browser"}, dir: "workspace", noBrowser: true},
		{name: "unknown flag", args: []string{"--unknown"}, wantErr: true},
		{name: "end options", args: []string{"--", "-workspace"}, dir: "-workspace"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, noBrowser, err := parseOpenArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseOpenArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if dir != tt.dir || noBrowser != tt.noBrowser {
				t.Errorf("parseOpenArgs() = %q, %v; want %q, %v", dir, noBrowser, tt.dir, tt.noBrowser)
			}
		})
	}
}
