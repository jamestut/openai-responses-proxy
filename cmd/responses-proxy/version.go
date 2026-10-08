package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// These defaults apply when a build does not supply linker values.
var version = "dev"
var buildTime = "unknown"

// printVersion reports VCS metadata embedded by the Go tool when available.
func printVersion() {
	commit := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				commit = setting.Value
				break
			}
		}
	}

	fmt.Printf("Version: %s\nCommit: %s\nBuild time: %s\nGo version: %s\n", version, commit, buildTime, runtime.Version())
}
