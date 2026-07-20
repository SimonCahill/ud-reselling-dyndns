package main

import "testing"

func TestApplicationVersionMetadata(t *testing.T) {
    originalVersion := version
    version = "v1.2.3"
    t.Cleanup(func() {
        version = originalVersion
    })

    if got := applicationVersion(); got != "v1.2.3" {
        t.Errorf("applicationVersion() = %q, want v1.2.3", got)
    }
    if got := applicationUserAgent(); got != "ud-reselling-dyndns/v1.2.3" {
        t.Errorf("applicationUserAgent() = %q, want versioned user agent", got)
    }
    if got := applicationVersionOutput(); got != "ud-reselling-dyndns v1.2.3" {
        t.Errorf("applicationVersionOutput() = %q, want versioned output", got)
    }
}

func TestApplicationVersionFallsBackToDevel(t *testing.T) {
    originalVersion := version
    version = "  "
    t.Cleanup(func() {
        version = originalVersion
    })

    if got := applicationVersion(); got != "devel" {
        t.Errorf("applicationVersion() = %q, want devel", got)
    }
}
