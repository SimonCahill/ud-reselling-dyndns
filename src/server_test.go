package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestBuildSubdomainUpdateFormPreservesUnmanagedRecords(t *testing.T) {
	t.Parallel()

	config := Config{User: "user", Password: "password"}
	domain := DomainConfig{
		Name:       "example.com",
		Subdomains: []string{"home.example.com"},
	}
	records := []dnsRecord{
		mustParseDNSRecord(t, "example.com. 600 IN MX 10 mail.example.com."),
		mustParseDNSRecord(t, "home.example.com. 300 IN A 192.0.2.1"),
		mustParseDNSRecord(t, "home.example.com. 300 IN AAAA 2001:db8::1"),
		mustParseDNSRecord(t, "home.example.com. 600 IN TXT fixed-entry"),
	}

	values := buildSubdomainUpdateForm(
		config,
		domain,
		"home.example.com",
		records,
		"192.0.2.10",
		"2001:db8::10",
	)

	wantRecords := map[string]string{
		"delrr0": "home.example.com. 300 IN A 192.0.2.1",
		"delrr1": "home.example.com. 300 IN AAAA 2001:db8::1",
		"addrr0": "home.example.com. 600 IN A 192.0.2.10",
		"addrr1": "home.example.com. 600 IN AAAA 2001:db8::10",
	}
	for key, want := range wantRecords {
		if got := values.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for key := range values {
		if strings.HasPrefix(key, "rr") {
			t.Errorf("destructive parameter %s must not be present", key)
		}
	}
	if got := values.Get("delrr2"); got != "" {
		t.Errorf("delrr2 = %q, want fixed records to remain untouched", got)
	}
}

func TestBuildSubdomainUpdateFormIncludesOnlyAvailableAddressFamilies(t *testing.T) {
	t.Parallel()

	config := Config{User: "user", Password: "password"}
	domain := DomainConfig{
		Name:       "example.com",
		Subdomains: []string{"home.example.com"},
	}
	records := []dnsRecord{
		mustParseDNSRecord(t, "home.example.com. 600 IN A 192.0.2.1"),
		mustParseDNSRecord(t, "home.example.com. 600 IN AAAA 2001:db8::1"),
	}

	tests := []struct {
		name       string
		ipv4       string
		ipv6       string
		wantAddrr0 string
	}{
		{
			name:       "IPv4 only",
			ipv4:       "192.0.2.10",
			wantAddrr0: "home.example.com. 600 IN A 192.0.2.10",
		},
		{
			name:       "IPv6 only",
			ipv6:       "2001:db8::10",
			wantAddrr0: "home.example.com. 600 IN AAAA 2001:db8::10",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			values := buildSubdomainUpdateForm(
				config,
				domain,
				"home.example.com",
				records,
				test.ipv4,
				test.ipv6,
			)
			if got := values.Get("addrr0"); got != test.wantAddrr0 {
				t.Errorf("addrr0 = %q, want %q", got, test.wantAddrr0)
			}
			if got := values.Get("addrr1"); got != "" {
				t.Errorf("addrr1 = %q, want no record for unavailable family", got)
			}
			if got := values.Get("delrr0"); got == "" {
				t.Error("delrr0 is empty, want existing A record deleted")
			}
			if got := values.Get("delrr1"); got == "" {
				t.Error("delrr1 is empty, want existing AAAA record deleted")
			}
		})
	}
}

func TestBuildSubdomainUpdateFormSkipsAlreadyCurrentRecords(t *testing.T) {
	t.Parallel()

	config := Config{User: "user", Password: "password"}
	domain := DomainConfig{
		Name:       "example.com",
		Subdomains: []string{"home.example.com"},
	}
	records := []dnsRecord{
		mustParseDNSRecord(t, "home.example.com. 600 IN A 192.0.2.10"),
		mustParseDNSRecord(t, "home.example.com. 600 IN AAAA 2001:db8::10"),
	}

	values := buildSubdomainUpdateForm(
		config,
		domain,
		"home.example.com",
		records,
		"192.0.2.10",
		"2001:db8::10",
	)

	if hasRecordChanges(values) {
		t.Errorf("buildSubdomainUpdateForm() = %v, want no record changes", values)
	}
}

func mustParseDNSRecord(t *testing.T, raw string) dnsRecord {
	t.Helper()
	record, err := parseDNSRecord(raw)
	if err != nil {
		t.Fatalf("parseDNSRecord(%q) error = %v", raw, err)
	}
	return record
}

func TestExternalIPsAcceptsOneAvailableAddressFamily(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ipv4Code int
		ipv6Code int
		wantIPv4 string
		wantIPv6 string
	}{
		{
			name:     "IPv4 only",
			ipv4Code: http.StatusOK,
			ipv6Code: http.StatusServiceUnavailable,
			wantIPv4: "192.0.2.10",
		},
		{
			name:     "IPv6 only",
			ipv4Code: http.StatusServiceUnavailable,
			ipv6Code: http.StatusOK,
			wantIPv6: "2001:db8::10",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client := &http.Client{
				Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					statusCode := test.ipv4Code
					body := "192.0.2.10"
					if request.URL.Path == "/ipv6" {
						statusCode = test.ipv6Code
						body = "2001:db8::10"
					}

					return &http.Response{
						StatusCode: statusCode,
						Status:     http.StatusText(statusCode),
						Body:       io.NopCloser(bytes.NewBufferString(body)),
						Header:     make(http.Header),
						Request:    request,
					}, nil
				}),
			}
			u := updater{
				ipv4URL:    "https://ip.example.test/ipv4",
				ipv6URL:    "https://ip.example.test/ipv6",
				httpClient: client,
			}

			ipv4, ipv6, err := u.externalIPs(context.Background())
			if err != nil {
				t.Fatalf("externalIPs() error = %v", err)
			}
			if ipv4 != test.wantIPv4 {
				t.Errorf("externalIPs() IPv4 = %q, want %q", ipv4, test.wantIPv4)
			}
			if ipv6 != test.wantIPv6 {
				t.Errorf("externalIPs() IPv6 = %q, want %q", ipv6, test.wantIPv6)
			}
		})
	}
}

func TestDisplayIPAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		address string
		want    string
	}{
		{address: "192.0.2.10", want: "192.0.2.10"},
		{address: "2001:db8::10", want: "2001:db8::10"},
		{address: "", want: "unavailable"},
	}

	for _, test := range tests {
		if got := displayIPAddress(test.address); got != test.want {
			t.Errorf("displayIPAddress(%q) = %q, want %q", test.address, got, test.want)
		}
	}
}

func TestLogConfiguredZonesLogsEveryOnlineRecord(t *testing.T) {
	var logs bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() {
		log.SetOutput(originalOutput)
	})

	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return apiHTTPResponse(request, apiSuccess(
				"example.com. 600 IN SOA ns.example.com. hostmaster.example.com. 1 3600 600 86400 600",
				"example.com. 600 IN MX 10 mail.example.com.",
				"home.example.com. 600 IN A 192.0.2.1",
			)), nil
		}),
	}
	u := updater{apiURL: "https://api.example.test/update", httpClient: client}
	config := Config{
		User:     "user",
		Password: "password",
		Domains: []DomainConfig{{
			Name:       "example.com",
			Subdomains: []string{"home.example.com"},
		}},
	}

	u.logConfiguredZones(context.Background(), config)

	wantLogs := []string{
		"Configured DNS zone entries at startup:",
		"\tDNS zone example.com:",
		"\t\texample.com. 600 IN SOA ns.example.com. hostmaster.example.com. 1 3600 600 86400 600",
		"\t\texample.com. 600 IN MX 10 mail.example.com.",
		"\t\thome.example.com. 600 IN A 192.0.2.1",
	}
	for _, want := range wantLogs {
		if got := logs.String(); !strings.Contains(got, want) {
			t.Errorf("log output = %q, want it to contain %q", got, want)
		}
	}
}

func TestUpdateDomainLogsSubdomainResults(t *testing.T) {
	var logs bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() {
		log.SetOutput(originalOutput)
	})

	config := Config{User: "user", Password: "password"}
	domain := DomainConfig{
		Name:       "Example.COM.",
		Subdomains: []string{"Home.Example.COM."},
	}

	var queryCount int
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := request.ParseForm(); err != nil {
				return nil, err
			}

			var body string
			switch request.PostForm.Get("command") {
			case "QueryDNSZoneRRList":
				queryCount++
				address := "192.0.2.1"
				if queryCount == 2 {
					address = "192.0.2.10"
				}
				body = apiSuccess(
					"example.com. 600 IN SOA ns.example.com. hostmaster.example.com. 1 3600 600 86400 600",
					"example.com. 600 IN MX 10 mail.example.com.",
					"home.example.com. 600 IN A "+address,
					"home.example.com. 600 IN AAAA 2001:db8::10",
				)
			case "UpdateDNSZone":
				if got := request.PostForm.Get("delrr0"); got != "home.example.com. 600 IN A 192.0.2.1" {
					t.Errorf("delrr0 = %q, want old A record", got)
				}
				if got := request.PostForm.Get("addrr0"); got != "home.example.com. 600 IN A 192.0.2.10" {
					t.Errorf("addrr0 = %q, want new A record", got)
				}
				for key := range request.PostForm {
					if strings.HasPrefix(key, "rr") {
						t.Errorf("destructive parameter %s must not be sent", key)
					}
				}
				body = apiSuccess()
			default:
				t.Fatalf("unexpected command %q", request.PostForm.Get("command"))
			}

			return apiHTTPResponse(request, body), nil
		}),
	}
	u := updater{apiURL: "https://api.example.test/update", httpClient: client}

	if err := u.updateDomain(
		context.Background(),
		config,
		domain,
		"192.0.2.10",
		"2001:db8::10",
	); err != nil {
		t.Fatalf("updateDomain() error = %v", err)
	}

	wantLogs := []string{
		"Updating DNS zone example.com...",
		"\tUpdating subdomain home.example.com to IPv4=192.0.2.10 IPv6=2001:db8::10... OK",
		"\tVerifying subdomain home.example.com online... OK",
	}
	for _, want := range wantLogs {
		if got := logs.String(); !strings.Contains(got, want) {
			t.Errorf("log output = %q, want it to contain %q", got, want)
		}
	}
}

func TestUpdateDomainFailsWhenOnlineZoneDoesNotContainChange(t *testing.T) {
	t.Parallel()

	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := request.ParseForm(); err != nil {
				return nil, err
			}
			if request.PostForm.Get("command") == "UpdateDNSZone" {
				return apiHTTPResponse(request, apiSuccess()), nil
			}
			return apiHTTPResponse(request, apiSuccess(
				"home.example.com. 600 IN A 192.0.2.1",
			)), nil
		}),
	}
	u := updater{apiURL: "https://api.example.test/update", httpClient: client}
	config := Config{User: "user", Password: "password"}
	domain := DomainConfig{
		Name:       "example.com",
		Subdomains: []string{"home.example.com"},
	}

	err := u.updateDomain(context.Background(), config, domain, "192.0.2.10", "")
	if err == nil || !strings.Contains(err.Error(), "A record is 192.0.2.1, want 192.0.2.10") {
		t.Fatalf("updateDomain() error = %v, want online verification error", err)
	}
}

func TestUpdateDomainSkipsAlreadyCurrentSubdomain(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	var queryCount int
	var updateCount int
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := request.ParseForm(); err != nil {
				return nil, err
			}
			switch request.PostForm.Get("command") {
			case "QueryDNSZoneRRList":
				queryCount++
				return apiHTTPResponse(request, apiSuccess(
					"home.example.com. 600 IN A 192.0.2.10",
					"home.example.com. 600 IN AAAA 2001:db8::10",
				)), nil
			case "UpdateDNSZone":
				updateCount++
				return apiHTTPResponse(request, apiSuccess()), nil
			default:
				t.Fatalf("unexpected command %q", request.PostForm.Get("command"))
				return nil, nil
			}
		}),
	}
	u := updater{
		apiURL:     "https://api.example.test/update",
		httpClient: client,
		logger:     log.New(&logs, "", 0),
	}
	config := Config{User: "user", Password: "password"}
	domain := DomainConfig{
		Name:       "example.com",
		Subdomains: []string{"home.example.com"},
	}

	if err := u.updateDomain(
		context.Background(),
		config,
		domain,
		"192.0.2.10",
		"2001:db8::10",
	); err != nil {
		t.Fatalf("updateDomain() error = %v", err)
	}

	if queryCount != 1 {
		t.Errorf("QueryDNSZoneRRList count = %d, want 1", queryCount)
	}
	if updateCount != 0 {
		t.Errorf("UpdateDNSZone count = %d, want 0", updateCount)
	}
	if got := logs.String(); !strings.Contains(got, "OK (already current)") {
		t.Errorf("log output = %q, want already-current result", got)
	}
}

func TestUpdateAllDomainsSendsOneRequestPerZone(t *testing.T) {
	t.Parallel()

	var updateZones []string
	var updateZonesMu sync.Mutex
	updatedZones := make(map[string]bool)
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := request.ParseForm(); err != nil {
				t.Errorf("ParseForm() error = %v", err)
				return nil, err
			}

			zone := request.PostForm.Get("dnszone")
			switch request.PostForm.Get("command") {
			case "QueryDNSZoneRRList":
				host := "home." + strings.TrimSuffix(zone, ".")
				ipv4 := "192.0.2.1"
				ipv6 := "2001:db8::1"
				updateZonesMu.Lock()
				if updatedZones[zone] {
					ipv4 = "192.0.2.10"
					ipv6 = "2001:db8::10"
				}
				updateZonesMu.Unlock()
				return apiHTTPResponse(request, apiSuccess(
					zone+" 600 IN SOA ns."+zone+" hostmaster."+zone+" 1 3600 600 86400 600",
					host+". 600 IN A "+ipv4,
					host+". 600 IN AAAA "+ipv6,
				)), nil
			case "UpdateDNSZone":
				updateZonesMu.Lock()
				updateZones = append(updateZones, zone)
				updatedZones[zone] = true
				updateZonesMu.Unlock()
				return apiHTTPResponse(request, apiSuccess()), nil
			default:
				t.Fatalf("unexpected command %q", request.PostForm.Get("command"))
				return nil, nil
			}
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

	if got := len(updateZones); got != 2 {
		t.Fatalf("update request count = %d, want 2", got)
	}
	if updateZones[0] != "example.com." || updateZones[1] != "example.org." {
		t.Errorf("updated zones = %v, want [example.com. example.org.]", updateZones)
	}
}

func TestCallAPIRejectsBodyLevelError(t *testing.T) {
	t.Parallel()

	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return apiHTTPResponse(request, "[RESPONSE]\ncode = 541\ndescription = Permission denied\nEOF\n"), nil
		}),
	}
	u := updater{apiURL: "https://api.example.test/update", httpClient: client}

	_, err := u.callAPI(context.Background(), url.Values{"command": {"UpdateDNSZone"}})
	if err == nil || !strings.Contains(err.Error(), "API code 541: Permission denied") {
		t.Fatalf("callAPI() error = %v, want API code error", err)
	}
}

func TestCallAPIAllowsLargeZoneResponse(t *testing.T) {
	t.Parallel()

	var body strings.Builder
	body.WriteString("[RESPONSE]\ncode = 200\ndescription = Command completed successfully\n")
	const recordCount = 2000
	for i := 0; i < recordCount; i++ {
		fmt.Fprintf(
			&body,
			"property[RR][%d] = host-%d.example.com. 600 IN A 192.0.2.10\n",
			i,
			i,
		)
	}
	body.WriteString("EOF\n")
	if body.Len() <= maxResponseBodySize {
		t.Fatalf("test response size = %d, want more than %d", body.Len(), maxResponseBodySize)
	}

	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return apiHTTPResponse(request, body.String()), nil
		}),
	}
	u := updater{apiURL: "https://api.example.test/update", httpClient: client}

	response, err := u.callAPI(context.Background(), url.Values{
		"command": {"QueryDNSZoneRRList"},
		"dnszone": {"example.com."},
	})
	if err != nil {
		t.Fatalf("callAPI() error = %v", err)
	}
	if got := len(response.Properties["rr"]); got != recordCount {
		t.Errorf("RR count = %d, want %d", got, recordCount)
	}
}

func TestParseAPIResponseAllowsLongPropertyLine(t *testing.T) {
	t.Parallel()

	value := strings.Repeat("x", maxResponseBodySize)
	body := "[RESPONSE]\ncode = 200\ndescription = Command completed successfully\n" +
		"property[RR][0] = example.com. 600 IN TXT " + value + "\nEOF\n"

	response, err := parseAPIResponse(body)
	if err != nil {
		t.Fatalf("parseAPIResponse() error = %v", err)
	}
	records := response.Properties["rr"]
	if len(records) != 1 {
		t.Fatalf("RR count = %d, want 1", len(records))
	}
	if !strings.HasSuffix(records[0], value) {
		t.Errorf("RR value length = %d, want long property value preserved", len(records[0]))
	}
}

func TestCallAPIRejectsResponseLargerThanCommandLimit(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("x", maxResponseBodySize+1)
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return apiHTTPResponse(request, body), nil
		}),
	}
	u := updater{apiURL: "https://api.example.test/update", httpClient: client}

	_, err := u.callAPI(context.Background(), url.Values{
		"command": {"UpdateDNSZone"},
		"dnszone": {"example.com."},
	})
	if err == nil || !strings.Contains(err.Error(), "response exceeds 65536-byte limit") {
		t.Fatalf("callAPI() error = %v, want response limit error", err)
	}
}

func apiSuccess(records ...string) string {
	var body strings.Builder
	body.WriteString("[RESPONSE]\ncode = 200\ndescription = Command completed successfully\n")
	for i, record := range records {
		fmt.Fprintf(&body, "property[RR][%d] = %s\n", i, record)
	}
	body.WriteString("EOF\n")
	return body.String()
}

func apiHTTPResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    request,
	}
}

func TestCallAPILogsChangesAndRedactedResponse(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return apiHTTPResponse(
				request,
				"[RESPONSE]\ncode = 200\ndescription = updated by reseller-user using secret\nEOF\n",
			), nil
		}),
	}
	u := updater{
		apiURL:     "https://api.example.test/update",
		httpClient: client,
		logger:     log.New(&logs, "", 0),
	}
	values := url.Values{
		"s_login": {"reseller-user"},
		"s_pw":    {"secret"},
		"command": {"UpdateDNSZone"},
		"dnszone": {"example.com."},
		"delrr0":  {"home.example.com. 600 IN A 192.0.2.1"},
		"addrr0":  {"home.example.com. 600 IN A 192.0.2.10"},
	}

	if _, err := u.callAPI(context.Background(), values); err != nil {
		t.Fatalf("callAPI() error = %v", err)
	}

	output := logs.String()
	for _, want := range []string{
		`Submitting UDR API request: command=UpdateDNSZone zone=example.com`,
		`delrr0="home.example.com. 600 IN A 192.0.2.1"`,
		`addrr0="home.example.com. 600 IN A 192.0.2.10"`,
		`status=200 OK`,
		`description = updated by [REDACTED] using [REDACTED]`,
		`UDR API request completed: command=UpdateDNSZone zone=example.com`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("logs do not contain %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "reseller-user") || strings.Contains(output, "secret") {
		t.Errorf("logs contain credentials:\n%s", output)
	}
}

func TestCallAPILogsHTTPFailure(t *testing.T) {
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

	_, err := u.callAPI(context.Background(), url.Values{
		"command": {"UpdateDNSZone"},
		"dnszone": {"example.com."},
	})
	if err == nil {
		t.Fatal("callAPI() error = nil, want an error")
	}

	output := logs.String()
	for _, want := range []string{
		`UDR API response: command=UpdateDNSZone zone=example.com status=400 Bad Request body="error=invalid zone"`,
		`UDR API request failed: command=UpdateDNSZone zone=example.com error=UpdateDNSZone: HTTP 400 Bad Request: error=invalid zone`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("logs do not contain %q:\n%s", want, output)
		}
	}
}

func TestCallAPILogsProviderFailureFromSuccessfulHTTPResponse(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return apiHTTPResponse(
				request,
				"[RESPONSE]\ncode = 541\ndescription = invalid credentials\nEOF\n",
			), nil
		}),
	}
	u := updater{
		apiURL:     "https://api.example.test/update",
		httpClient: client,
		logger:     log.New(&logs, "", 0),
	}

	_, err := u.callAPI(context.Background(), url.Values{
		"command": {"UpdateDNSZone"},
		"dnszone": {"example.com."},
	})
	if err == nil {
		t.Fatal("callAPI() error = nil, want an error")
	}

	output := logs.String()
	if !strings.Contains(output, "API code 541: invalid credentials") {
		t.Errorf("provider failure was not logged:\n%s", output)
	}
	if strings.Contains(output, "request completed:") {
		t.Errorf("provider failure was logged as completed:\n%s", output)
	}
}

func TestCallAPILogsTransportFailure(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("connection refused")
		}),
	}
	u := updater{
		apiURL:     "https://api.example.test/update",
		httpClient: client,
		logger:     log.New(&logs, "", 0),
	}

	if _, err := u.callAPI(context.Background(), url.Values{
		"command": {"UpdateDNSZone"},
		"dnszone": {"example.com."},
	}); err == nil {
		t.Fatal("callAPI() error = nil, want an error")
	}
	if output := logs.String(); !strings.Contains(output, "UDR API request failed: command=UpdateDNSZone zone=example.com") ||
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
