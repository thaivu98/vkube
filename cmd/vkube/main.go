package main

import (
	"fmt"
	"runtime"
)

var (
	version = "v1.32.2-vkube.1-dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	fmt.Printf("vkube %s (commit: %s, built: %s, %s/%s)\n",
		version, commit, date, runtime.GOOS, runtime.GOARCH)
}
