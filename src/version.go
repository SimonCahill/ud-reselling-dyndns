package main

import "strings"

const applicationName = "ud-reselling-dyndns"

// version is replaced at build time with "-X main.version=<version>". Keeping
// a useful fallback makes direct "go build" invocations self-describing.
var version = "devel"

// applicationVersion returns a safe value even when a build system passes an
// empty linker value.
func applicationVersion() string {
    if value := strings.TrimSpace(version); value != "" {
        return value
    }
    return "devel"
}

// applicationUserAgent identifies this exact client build to DynDNS v2
// providers, as required by the protocol.
func applicationUserAgent() string {
    return applicationName + "/" + applicationVersion()
}

// applicationVersionOutput is the stable human-readable form printed by the
// command-line version flag.
func applicationVersionOutput() string {
    return applicationName + " " + applicationVersion()
}
