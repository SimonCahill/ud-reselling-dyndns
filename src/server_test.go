package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
				"subdomains": ["home.example.com", "vpn.example.com"]
			},
			{
				"name": "example.org.",
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
			Name:       "example.com",
			Subdomains: []string{"home.example.org"},
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
		Name:       "example.com",
		Subdomains: []string{"home.example.com", "vpn.example.com"},
	}

	values := buildUpdateForm(config, domain, "192.0.2.10", "2001:db8::10")

	wantRecords := map[string]string{
		"rr0": "home.example.com. 600 IN A 192.0.2.10",
		"rr1": "home.example.com. 600 IN AAAA 2001:db8::10",
		"rr2": "vpn.example.com. 600 IN A 192.0.2.10",
		"rr3": "vpn.example.com. 600 IN AAAA 2001:db8::10",
	}
	for key, want := range wantRecords {
		if got := values.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := values.Get("rr4"); got != "" {
		t.Errorf("rr4 = %q, want no additional records", got)
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
			{Name: "example.com", Subdomains: []string{"home.example.com"}},
			{Name: "example.org", Subdomains: []string{"home.example.org"}},
		},
	}
	u := updater{apiURL: "https://api.example.test/update", httpClient: client}

	if err := u.updateAllDomains(context.Background(), config, "192.0.2.10", "2001:db8::10"); err != nil {
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

func TestUpdateDomainLogsChangesAndResponse(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body: io.NopCloser(strings.NewReader(
					"result=success\nmessage=updated by reseller-user using secret",
				)),
				Header:  make(http.Header),
				Request: request,
			}, nil
		}),
	}
	u := updater{
		apiURL:     "https://api.example.test/update",
		httpClient: client,
		logger:     log.New(&logs, "", 0),
	}
	config := Config{User: "reseller-user", Password: "secret"}
	domain := DomainConfig{Name: "example.com", Subdomains: []string{"home.example.com"}}

	if err := u.updateDomain(context.Background(), config, domain, "192.0.2.10", "2001:db8::10"); err != nil {
		t.Fatalf("updateDomain() error = %v", err)
	}

	output := logs.String()
	for _, want := range []string{
		`Submitting UDR DNS update: zone=example.com records=2`,
		`rr0="home.example.com. 600 IN A 192.0.2.10"`,
		`rr1="home.example.com. 600 IN AAAA 2001:db8::10"`,
		`status=200 OK`,
		`body="result=success message=updated by [REDACTED] using [REDACTED]"`,
		`UDR DNS update request completed: zone=example.com records=2`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("logs do not contain %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, config.User) || strings.Contains(output, config.Password) {
		t.Errorf("logs contain credentials:\n%s", output)
	}
}

func TestUpdateDomainLogsHTTPFailure(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Status:     "400 Bad Request",
				Body:       io.NopCloser(strings.NewReader("error=invalid zone")),
				Header:     make(http.Header),
				Request:    request,
			}, nil
		}),
	}
	u := updater{
		apiURL:     "https://api.example.test/update",
		httpClient: client,
		logger:     log.New(&logs, "", 0),
	}
	config := Config{User: "user", Password: "password"}
	domain := DomainConfig{Name: "example.com", Subdomains: []string{"home.example.com"}}

	err := u.updateDomain(context.Background(), config, domain, "192.0.2.10", "2001:db8::10")
	if err == nil {
		t.Fatal("updateDomain() error = nil, want an error")
	}

	output := logs.String()
	for _, want := range []string{
		`UDR DNS response: zone=example.com status=400 Bad Request body="error=invalid zone"`,
		`UDR DNS update failed: zone=example.com error=HTTP 400 Bad Request: error=invalid zone`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("logs do not contain %q:\n%s", want, output)
		}
	}
}

func TestUpdateDomainLogsProviderFailureFromSuccessfulHTTPResponse(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(`{"success":false,"error":"invalid credentials"}`)),
				Header:     make(http.Header),
				Request:    request,
			}, nil
		}),
	}
	u := updater{
		apiURL:     "https://api.example.test/update",
		httpClient: client,
		logger:     log.New(&logs, "", 0),
	}
	config := Config{User: "user", Password: "password"}
	domain := DomainConfig{Name: "example.com", Subdomains: []string{"home.example.com"}}

	err := u.updateDomain(context.Background(), config, domain, "192.0.2.10", "2001:db8::10")
	if err == nil {
		t.Fatal("updateDomain() error = nil, want an error")
	}

	output := logs.String()
	if !strings.Contains(output, "UDR reported failure") {
		t.Errorf("provider failure was not logged:\n%s", output)
	}
	if strings.Contains(output, "request completed") {
		t.Errorf("provider failure was logged as completed:\n%s", output)
	}
}

func TestUpdateDomainLogsTransportFailure(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		}),
	}
	u := updater{
		apiURL:     "https://api.example.test/update",
		httpClient: client,
		logger:     log.New(&logs, "", 0),
	}
	config := Config{User: "user", Password: "password"}
	domain := DomainConfig{Name: "example.com", Subdomains: []string{"home.example.com"}}

	if err := u.updateDomain(context.Background(), config, domain, "192.0.2.10", "2001:db8::10"); err == nil {
		t.Fatal("updateDomain() error = nil, want an error")
	}
	if output := logs.String(); !strings.Contains(output, "UDR DNS update failed: zone=example.com") ||
		!strings.Contains(output, "connection refused") {
		t.Errorf("failure was not logged:\n%s", output)
	}
}

func TestConfigJSONHasNoUnknownFields(t *testing.T) {
	t.Parallel()

	var config Config
	if err := json.Unmarshal([]byte(`{"user":"u","password":"p","domains":[]}`), &config); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
}
