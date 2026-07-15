package main

import (
    "bytes"
    "context"
    "io"
    "log"
    "net/http"
    "net/url"
    "os"
    "path/filepath"
    "strings"
    "testing"
    "time"
)

func TestReadConfigKeepsLegacyConfigurationCompatible(t *testing.T) {
    t.Parallel()

    configPath := filepath.Join(t.TempDir(), "config.json")
    legacy := `{
        "user": "reseller-user",
        "password": "secret",
        "pollInterval": "30s",
        "domains": [{"name":"example.com","subdomains":["home.example.com"]}]
    }`
    if err := os.WriteFile(configPath, []byte(legacy), 0o600); err != nil {
        t.Fatal(err)
    }

    config, err := readConfig(configPath)
    if err != nil {
        t.Fatalf("readConfig() error = %v", err)
    }
    if config.User != "reseller-user" || len(config.Domains) != 1 || len(config.Providers) != 0 {
        t.Fatalf("readConfig() = %+v, want unchanged legacy fields", config)
    }

    u := updater{httpClient: http.DefaultClient}
    providers, err := newRuntimeProviders(config, u)
    if err != nil {
        t.Fatalf("newRuntimeProviders() error = %v", err)
    }
    if len(providers) != 1 || providers[0].name() != "legacy-udr" {
        t.Fatalf("providers = %#v, want one legacy UDR provider", providers)
    }
    contents, err := os.ReadFile(configPath)
    if err != nil {
        t.Fatal(err)
    }
    if string(contents) != legacy {
        t.Fatal("readConfig() rewrote the legacy configuration")
    }
}

func TestReadConfigSupportsMixedProviders(t *testing.T) {
    t.Parallel()

    configPath := filepath.Join(t.TempDir(), "config.json")
    canonical := `{
        "pollInterval": "1m",
        "providers": [
            {
                "name":"primary-udr", "type":"udr", "user":"u", "password":"p",
                "domains":[{"name":"example.com","subdomains":["home.example.com"]}]
            },
            {
                "name":"secondary", "type":"dyndns2", "user":"u2", "password":"p2",
                "updateUrl":"https://ddns.example/nic/update?token=fixed",
                "addressFamily":"ipv6", "hostnames":["home.example.net."]
            }
        ]
    }`
    if err := os.WriteFile(configPath, []byte(canonical), 0o600); err != nil {
        t.Fatal(err)
    }

    config, err := readConfig(configPath)
    if err != nil {
        t.Fatalf("readConfig() error = %v", err)
    }
    if got := len(config.Providers); got != 2 {
        t.Fatalf("len(Providers) = %d, want 2", got)
    }
    if config.Providers[1].AddressFamily != "ipv6" {
        t.Errorf("AddressFamily = %q, want ipv6", config.Providers[1].AddressFamily)
    }
}

func TestProviderConfigValidation(t *testing.T) {
    t.Parallel()

    validDynDNS := ProviderConfig{
        Name:      "ddns",
        Type:      providerTypeDynDNS2,
        User:      "user",
        Password:  "password",
        UpdateURL: "https://ddns.example/nic/update",
        Hostnames: []string{"home.example.com"},
    }
    tests := []struct {
        name   string
        config Config
        want   string
    }{
        {
            name:   "legacy and providers cannot mix",
            config: Config{User: "legacy", Providers: []ProviderConfig{validDynDNS}},
            want:   "cannot be combined",
        },
        {
            name: "duplicate provider names",
            config: Config{Providers: []ProviderConfig{validDynDNS, func() ProviderConfig {
                duplicate := validDynDNS
                duplicate.Name = "DDNS"
                return duplicate
            }()}},
            want: "configured more than once",
        },
        {
            name: "HTTPS is required",
            config: Config{Providers: []ProviderConfig{func() ProviderConfig {
                provider := validDynDNS
                provider.UpdateURL = "http://ddns.example/nic/update"
                return provider
            }()}},
            want: "absolute HTTPS URL",
        },
        {
            name: "address family is strict",
            config: Config{Providers: []ProviderConfig{func() ProviderConfig {
                provider := validDynDNS
                provider.AddressFamily = "auto"
                return provider
            }()}},
            want: "unsupported addressFamily",
        },
        {
            name: "duplicate hostname",
            config: Config{Providers: []ProviderConfig{func() ProviderConfig {
                provider := validDynDNS
                provider.Hostnames = []string{"home.example.com", "HOME.EXAMPLE.COM."}
                return provider
            }()}},
            want: "configured more than once",
        },
    }

    for _, test := range tests {
        test := test
        t.Run(test.name, func(t *testing.T) {
            t.Parallel()
            err := test.config.validate()
            if err == nil || !strings.Contains(err.Error(), test.want) {
                t.Fatalf("validate() error = %v, want it to contain %q", err, test.want)
            }
        })
    }
}

func TestReadConfigRejectsUnknownProviderField(t *testing.T) {
    t.Parallel()

    configPath := filepath.Join(t.TempDir(), "config.json")
    contents := `{"providers":[{"name":"ddns","type":"dyndns2","user":"u","password":"p","updateUrl":"https://ddns.example/nic/update","hostnames":["home.example.com"],"token":"unexpected"}]}`
    if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
        t.Fatal(err)
    }
    if _, err := readConfig(configPath); err == nil || !strings.Contains(err.Error(), "unknown field") {
        t.Fatalf("readConfig() error = %v, want unknown-field error", err)
    }
}

func TestReadConfigRejectsFieldsFromAnotherProviderType(t *testing.T) {
    t.Parallel()

    configPath := filepath.Join(t.TempDir(), "config.json")
    contents := `{"providers":[{"name":"udr","type":"udr","user":"u","password":"p","domains":[{"name":"example.com","subdomains":["home.example.com"]}],"hostnames":[]}]}`
    if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
        t.Fatal(err)
    }
    if _, err := readConfig(configPath); err == nil || !strings.Contains(err.Error(), "not valid for an udr provider") {
        t.Fatalf("readConfig() error = %v, want provider-specific field error", err)
    }
}

func TestDynDNS2ProviderSendsOneAuthenticatedRequestPerHostname(t *testing.T) {
    t.Parallel()

    var requests []*http.Request
    client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
        requests = append(requests, request)
        return dynDNSHTTPResponse(request, "good 192.0.2.10,2001:db8::10"), nil
    })}
    provider := newTestDynDNS2Provider(client, []string{"home.example.com", "vpn.example.com"})

    if err := provider.update(context.Background(), "192.0.2.10", "2001:db8::10"); err != nil {
        t.Fatalf("update() error = %v", err)
    }
    if got := len(requests); got != 2 {
        t.Fatalf("request count = %d, want 2", got)
    }
    for i, request := range requests {
        if request.Method != http.MethodGet {
            t.Errorf("request[%d].Method = %s, want GET", i, request.Method)
        }
        if got := request.URL.Query().Get("myip"); got != "192.0.2.10,2001:db8::10" {
            t.Errorf("request[%d] myip = %q", i, got)
        }
        if got := request.URL.Query().Get("fixed"); got != "value" {
            t.Errorf("request[%d] fixed query = %q, want value", i, got)
        }
        user, password, ok := request.BasicAuth()
        if !ok || user != "user" || password != "password" {
            t.Errorf("request[%d] BasicAuth = %q, %q, %v", i, user, password, ok)
        }
        if request.UserAgent() != dynDNS2UserAgent {
            t.Errorf("request[%d] User-Agent = %q", i, request.UserAgent())
        }
    }
    if requests[0].URL.Query().Get("hostname") != "home.example.com" ||
        requests[1].URL.Query().Get("hostname") != "vpn.example.com" {
        t.Errorf("hostnames were not sent independently")
    }

    if err := provider.update(context.Background(), "192.0.2.10", "2001:db8::10"); err != nil {
        t.Fatalf("second update() error = %v", err)
    }
    if got := len(requests); got != 2 {
        t.Errorf("unchanged request count = %d, want 2", got)
    }
}

func TestDynDNS2AddressFamilySelection(t *testing.T) {
    t.Parallel()

    provider := newTestDynDNS2Provider(http.DefaultClient, []string{"home.example.com"})
    tests := []struct {
        family string
        ipv4   string
        ipv6   string
        want   string
    }{
        {family: addressFamilyIPv4, ipv4: "192.0.2.10", ipv6: "2001:db8::10", want: "192.0.2.10"},
        {family: addressFamilyIPv6, ipv4: "192.0.2.10", ipv6: "2001:db8::10", want: "2001:db8::10"},
        {family: addressFamilyBoth, ipv4: "192.0.2.10", ipv6: "2001:db8::10", want: "192.0.2.10,2001:db8::10"},
        {family: addressFamilyBoth, ipv6: "2001:db8::10", want: "2001:db8::10"},
    }
    for _, test := range tests {
        provider.addressFamily = test.family
        if got := provider.selectedAddresses(test.ipv4, test.ipv6); got != test.want {
            t.Errorf("selectedAddresses(%s) = %q, want %q", test.family, got, test.want)
        }
    }
}

func TestDynDNS2FatalHostnameDoesNotBlockSuccessfulHostname(t *testing.T) {
    t.Parallel()

    requestCount := make(map[string]int)
    client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
        hostname := request.URL.Query().Get("hostname")
        requestCount[hostname]++
        if hostname == "missing.example.com" {
            return dynDNSHTTPResponse(request, "nohost"), nil
        }
        return dynDNSHTTPResponse(request, "good 192.0.2.10"), nil
    })}
    provider := newTestDynDNS2Provider(client, []string{"missing.example.com", "home.example.com"})

    if err := provider.update(context.Background(), "192.0.2.10", ""); err == nil {
        t.Fatal("update() error = nil, want nohost error")
    }
    _ = provider.update(context.Background(), "192.0.2.10", "")
    if requestCount["missing.example.com"] != 1 || requestCount["home.example.com"] != 1 {
        t.Fatalf("unchanged request counts = %v, want both 1", requestCount)
    }

    if err := provider.update(context.Background(), "192.0.2.11", ""); err != nil {
        t.Fatalf("changed update() error = %v", err)
    }
    if requestCount["missing.example.com"] != 1 || requestCount["home.example.com"] != 2 {
        t.Fatalf("changed request counts = %v, want disabled=1 successful=2", requestCount)
    }
}

func TestDynDNS2TemporaryFailureUsesThirtyMinuteCooldown(t *testing.T) {
    t.Parallel()

    now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
    requestCount := 0
    client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
        requestCount++
        if requestCount == 1 {
            return dynDNSHTTPResponse(request, "911"), nil
        }
        return dynDNSHTTPResponse(request, "good 192.0.2.10"), nil
    })}
    provider := newTestDynDNS2Provider(client, []string{"home.example.com"})
    provider.now = func() time.Time { return now }

    if err := provider.update(context.Background(), "192.0.2.10", ""); err == nil {
        t.Fatal("update() error = nil, want temporary failure")
    }
    now = now.Add(29 * time.Minute)
    _ = provider.update(context.Background(), "192.0.2.10", "")
    if requestCount != 1 {
        t.Fatalf("request count during cooldown = %d, want 1", requestCount)
    }
    now = now.Add(time.Minute)
    if err := provider.update(context.Background(), "192.0.2.10", ""); err != nil {
        t.Fatalf("update after cooldown error = %v", err)
    }
    if requestCount != 2 {
        t.Fatalf("request count after cooldown = %d, want 2", requestCount)
    }
}

func TestParseDynDNS2Response(t *testing.T) {
    t.Parallel()

    tests := []struct {
        body  string
        scope dynDNS2OutcomeScope
    }{
        {body: "good 192.0.2.10", scope: dynDNS2Success},
        {body: "nochg 192.0.2.10", scope: dynDNS2Success},
        {body: "badauth", scope: dynDNS2DisableProvider},
        {body: "badagent", scope: dynDNS2DisableProvider},
        {body: "nohost", scope: dynDNS2DisableHostname},
        {body: "911", scope: dynDNS2Temporary},
        {body: "dnserr", scope: dynDNS2Temporary},
        {body: "good 192.0.2.10\nnochg 192.0.2.10", scope: dynDNS2DisableHostname},
        {body: "unexpected", scope: dynDNS2DisableHostname},
    }
    for _, test := range tests {
        if got := parseDynDNS2Response(test.body); got.scope != test.scope {
            t.Errorf("parseDynDNS2Response(%q).scope = %v, want %v", test.body, got.scope, test.scope)
        }
    }
}

func TestDynDNS2HTTPFailureClassification(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name       string
        statusCode int
        body       string
        wantScope  dynDNS2OutcomeScope
        wantError  bool
    }{
        {name: "unauthorized disables provider", statusCode: http.StatusUnauthorized, body: "unauthorized", wantScope: dynDNS2DisableProvider},
        {name: "bad request disables hostname", statusCode: http.StatusBadRequest, body: "bad request", wantScope: dynDNS2DisableHostname},
        {name: "server error retries", statusCode: http.StatusServiceUnavailable, body: "maintenance", wantError: true},
    }

    for _, test := range tests {
        test := test
        t.Run(test.name, func(t *testing.T) {
            t.Parallel()
            client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
                response := dynDNSHTTPResponse(request, test.body)
                response.StatusCode = test.statusCode
                response.Status = http.StatusText(test.statusCode)
                return response, nil
            })}
            provider := newTestDynDNS2Provider(client, []string{"home.example.com"})
            outcome, err := provider.updateHostname(context.Background(), "home.example.com", "192.0.2.10")
            if test.wantError {
                if err == nil {
                    t.Fatal("updateHostname() error = nil, want retryable error")
                }
                return
            }
            if err != nil {
                t.Fatalf("updateHostname() error = %v", err)
            }
            if outcome.scope != test.wantScope {
                t.Errorf("outcome.scope = %v, want %v", outcome.scope, test.wantScope)
            }
        })
    }
}

func TestDynDNS2LogsRedactCredentials(t *testing.T) {
    t.Parallel()

    var logs bytes.Buffer
    client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
        return dynDNSHTTPResponse(request, "unknown user password"), nil
    })}
    provider := newTestDynDNS2Provider(client, []string{"home.example.com"})
    provider.updater.logger = log.New(&logs, "", 0)
    _ = provider.update(context.Background(), "192.0.2.10", "")
    if strings.Contains(logs.String(), "user") || strings.Contains(logs.String(), "password") {
        t.Fatalf("logs contain credentials: %s", logs.String())
    }
}

func newTestDynDNS2Provider(client *http.Client, hostnames []string) *dynDNS2Provider {
    endpoint, _ := urlParseForTest("https://ddns.example/nic/update?fixed=value")
    targets := make([]dynDNS2Target, 0, len(hostnames))
    for _, hostname := range hostnames {
        targets = append(targets, dynDNS2Target{hostname: hostname})
    }
    return &dynDNS2Provider{
        providerName:  "test-provider",
        endpoint:      endpoint,
        user:          "user",
        password:      "password",
        addressFamily: addressFamilyBoth,
        targets:       targets,
        httpClient:    client,
        updater:       updater{logger: log.New(io.Discard, "", 0)},
        now:           time.Now,
    }
}

func urlParseForTest(rawURL string) (*url.URL, error) {
    return url.Parse(rawURL)
}

func dynDNSHTTPResponse(request *http.Request, body string) *http.Response {
    return &http.Response{
        StatusCode: http.StatusOK,
        Status:     "200 OK",
        Body:       io.NopCloser(strings.NewReader(body)),
        Header:     make(http.Header),
        Request:    request,
    }
}
