package main

import "untis-proxy/internal/server"

// Overridden by release builds of the standalone server.
var buildVersion = "dev"

func main() {
	server.Run(buildVersion)
}
