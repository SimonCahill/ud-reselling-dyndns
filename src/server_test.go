package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestReadConfigSupportsMultipleDomainsAndSubdomains(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	configJSON := `{
		"user": "reseller-user",
		"password": "secret",
		"pollInterval": "30s",
		"domains": [
			{
				"name": "example.com",
				"cnameMaster": "target.example.net",
				"subdomains": ["home.example.com", "vpn.example.com"]
			},
			{
				"name": "example.org.",
				"cnameMaster": "target.example.net.",
				"subdomains": ["home.example.org."]
			}
		]
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := readConfig(configPath)
	if err != nil {
		t.Fatalf("readConfig() error = %v", err)
	}

	if got, want := len(config.Domains), 2; got != want {
		t.Fatalf("len(config.Domains) = %d, want %d", got, want)
	}
	if got, want := len(config.Domains[0].Subdomains), 2; got != want {
		t.Fatalf("len(config.Domains[0].Subdomains) = %d, want %d", got, want)
	}
}

func TestConfigRejectsSubdomainOutsideZone(t *testing.T) {
	t.Parallel()

	config := Config{
		User:     "user",
		Password: "password",
		Domains: []DomainConfig{{
			Name:        "example.com",
			CNAMEMaster: "target.example.net",
			Subdomains:  []string{"home.example.org"},
		}},
	}

	if err := config.validate(); err == nil {
		t.Fatal("validate() error = nil, want an error")
	}
}

func TestBuildUpdateFormIncludesEverySubdomain(t *testing.T) {
	t.Parallel()

	config := Config{User: "user", Password: "password"}
	domain := DomainConfig{
		Name:        "example.com",
		CNAMEMaster: "target.example.net",
		Subdomains:  []string{"home.example.com", "vpn.example.com"},
	}
	updatedAt := time.Date(2026, time.June, 14, 12, 0, 0, 0, time.UTC)

	values := buildUpdateForm(config, domain, "192.0.2.10", "2001:db8::10", updatedAt)

	wantRecords := map[string]string{
		"rr0": "example.com. 3600 IN CNAME target.example.net.",
		"rr1": "*.example.com. 3600 IN CNAME target.example.net.",
		"rr2": "home.example.com. 600 IN A 192.0.2.10",
		"rr3": "home.example.com. 600 IN AAAA 2001:db8::10",
		"rr4": "home.example.com. 600 IN TXT 2026-06-14T12:00:00Z",
		"rr5": "vpn.example.com. 600 IN A 192.0.2.10",
		"rr6": "vpn.example.com. 600 IN AAAA 2001:db8::10",
		"rr7": "vpn.example.com. 600 IN TXT 2026-06-14T12:00:00Z",
	}
	for key, want := range wantRecords {
		if got := values.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestUpdateAllDomainsSendsOneRequestPerZone(t *testing.T) {
	t.Parallel()

	var requests []url.Values
	var requestsMu sync.Mutex
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := request.ParseForm(); err != nil {
				t.Errorf("ParseForm() error = %v", err)
				return nil, err
			}

			requestsMu.Lock()
			requests = append(requests, request.PostForm)
			requestsMu.Unlock()

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     make(http.Header),
				Request:    request,
			}, nil
		}),
	}

	config := Config{
		User:     "user",
		Password: "password",
		Domains: []DomainConfig{
			{Name: "example.com", CNAMEMaster: "target.example.net", Subdomains: []string{"home.example.com"}},
			{Name: "example.org", CNAMEMaster: "target.example.net", Subdomains: []string{"home.example.org"}},
		},
	}
	u := updater{apiURL: "https://api.example.test/update", httpClient: client}

	if err := u.updateAllDomains(context.Background(), config, "192.0.2.10", "2001:db8::10", time.Now()); err != nil {
		t.Fatalf("updateAllDomains() error = %v", err)
	}

	if got, want := len(requests), 2; got != want {
		t.Fatalf("request count = %d, want %d", got, want)
	}
	if got, want := requests[0].Get("dnszone"), "example.com."; got != want {
		t.Errorf("first dnszone = %q, want %q", got, want)
	}
	if got, want := requests[1].Get("dnszone"), "example.org."; got != want {
		t.Errorf("second dnszone = %q, want %q", got, want)
	}
}

func TestConfigJSONHasNoUnknownFields(t *testing.T) {
	t.Parallel()

	var config Config
	if err := json.Unmarshal([]byte(`{"user":"u","password":"p","domains":[]}`), &config); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
}
