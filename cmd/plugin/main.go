package main

import (
	"os"

	pluginv1 "sub2api.local/cache-manager/internal/hostapi"
	"sub2api.local/cache-manager/internal/plugin"
	_ "time/tzdata"
)

var version = "0.1.2-Beta1"

func main() {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}
	server := plugin.New(version, hostname)
	defer server.Close()
	pluginv1.Serve(server)
}
