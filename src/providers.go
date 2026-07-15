package main

import (
    "context"
    "errors"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "strings"
    "time"
)

const (
    // DynDNS responses are deliberately small; bounding them protects the
    // long-running client from a broken or malicious endpoint.
    dynDNS2ResponseLimit = 4 * 1024
    // The DynDNS v2 specification requires a substantial pause after server
    // failure responses such as 911 and dnserr.
    dynDNS2Cooldown      = 30 * time.Minute
    dynDNS2UserAgent     = "ud-reselling-dyndns/1"
)

// publishedAddresses is the last address pair successfully sent by a provider.
type publishedAddresses struct {
    ipv4        string
    ipv6        string
    initialized bool
}

// runtimeProvider is the common lifecycle used by the polling loop. Provider
// implementations own their retry and successful-publication state.
type runtimeProvider interface {
    name() string
    logConfigured(context.Context)
    update(context.Context, string, string) error
}

// udrProvider adapts the original UDR updater to the multi-provider runtime.
type udrProvider struct {
    providerName string
    config       Config
    updater      updater
    last         publishedAddresses
}

func (provider *udrProvider) name() string { return provider.providerName }

func (provider *udrProvider) logConfigured(ctx context.Context) {
    provider.updater.logConfiguredZones(ctx, provider.config)
}

// update preserves the original all-zones-success requirement: its cached
// address pair advances only after every configured UDR zone succeeds.
func (provider *udrProvider) update(ctx context.Context, ipv4, ipv6 string) error {
    if provider.last.initialized && provider.last.ipv4 == ipv4 && provider.last.ipv6 == ipv6 {
        return nil
    }
    if err := provider.updater.updateAllDomains(ctx, provider.config, ipv4, ipv6); err != nil {
        return err
    }
    provider.last = publishedAddresses{ipv4: ipv4, ipv6: ipv6, initialized: true}
    return nil
}

// dynDNS2Target stores independent state for one hostname. Isolation prevents
// one bad hostname from causing successful hostnames to be updated repeatedly.
type dynDNS2Target struct {
    hostname      string
    lastMyIP      string
    initialized   bool
    disabled      bool
    cooldownUntil time.Time
}

// dynDNS2Provider sends standard Members NIC Update requests to one endpoint.
type dynDNS2Provider struct {
    providerName  string
    endpoint      *url.URL
    user          string
    password      string
    addressFamily string
    targets       []dynDNS2Target
    httpClient    *http.Client
    updater       updater
    now           func() time.Time
    disabled      bool
}

func (provider *dynDNS2Provider) name() string { return provider.providerName }

func (provider *dynDNS2Provider) logConfigured(context.Context) {
    hostnames := make([]string, 0, len(provider.targets))
    for _, target := range provider.targets {
        hostnames = append(hostnames, target.hostname)
    }
    provider.updater.logf(
        "Configured DynDNS v2 provider %s: endpoint=%s addressFamily=%s hostnames=%s",
        provider.providerName,
        provider.endpoint.Redacted(),
        provider.addressFamily,
        strings.Join(hostnames, ","),
    )
}

// update publishes the selected addresses to every target that is eligible.
// Permanent protocol failures disable only their documented scope, while
// transient transport errors remain eligible on the next polling cycle.
func (provider *dynDNS2Provider) update(ctx context.Context, ipv4, ipv6 string) error {
    if provider.disabled {
        return nil
    }
    myIP := provider.selectedAddresses(ipv4, ipv6)
    if myIP == "" {
        return nil
    }

    var updateErrors []error
    for i := range provider.targets {
        target := &provider.targets[i]
        if target.disabled || (target.initialized && target.lastMyIP == myIP) {
            continue
        }
        if provider.now().Before(target.cooldownUntil) {
            continue
        }

        outcome, err := provider.updateHostname(ctx, target.hostname, myIP)
        if err != nil {
            updateErrors = append(updateErrors, fmt.Errorf("update hostname %s: %w", target.hostname, err))
            continue
        }

        switch outcome.scope {
        case dynDNS2Success:
            target.lastMyIP = myIP
            target.initialized = true
        case dynDNS2DisableHostname:
            target.disabled = true
            updateErrors = append(updateErrors, fmt.Errorf("hostname %s disabled until restart: %s", target.hostname, outcome.message))
        case dynDNS2DisableProvider:
            provider.disabled = true
            updateErrors = append(updateErrors, fmt.Errorf("provider disabled until restart: %s", outcome.message))
            return errors.Join(updateErrors...)
        case dynDNS2Temporary:
            target.cooldownUntil = provider.now().Add(dynDNS2Cooldown)
            updateErrors = append(updateErrors, fmt.Errorf("hostname %s suspended for %s: %s", target.hostname, dynDNS2Cooldown, outcome.message))
        }
    }
    return errors.Join(updateErrors...)
}

// selectedAddresses formats the myip parameter in IPv4-then-IPv6 order.
func (provider *dynDNS2Provider) selectedAddresses(ipv4, ipv6 string) string {
    switch provider.addressFamily {
    case addressFamilyIPv4:
        return ipv4
    case addressFamilyIPv6:
        return ipv6
    default:
        addresses := make([]string, 0, 2)
        if ipv4 != "" {
            addresses = append(addresses, ipv4)
        }
        if ipv6 != "" {
            addresses = append(addresses, ipv6)
        }
        return strings.Join(addresses, ",")
    }
}

// dynDNS2OutcomeScope describes how a protocol response affects future work.
type dynDNS2OutcomeScope int

const (
    dynDNS2Success dynDNS2OutcomeScope = iota
    dynDNS2DisableHostname
    dynDNS2DisableProvider
    dynDNS2Temporary
)

// dynDNS2Outcome is the normalized result of one HTTP response.
type dynDNS2Outcome struct {
    scope   dynDNS2OutcomeScope
    message string
}

// updateHostname performs one authenticated request. Hostnames are deliberately
// not batched so a hostname-specific response can be handled independently.
func (provider *dynDNS2Provider) updateHostname(
    ctx context.Context,
    hostname string,
    myIP string,
) (dynDNS2Outcome, error) {
    endpoint := *provider.endpoint
    query := endpoint.Query()
    query.Set("hostname", hostname)
    query.Set("myip", myIP)
    endpoint.RawQuery = query.Encode()

    request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
    if err != nil {
        return dynDNS2Outcome{}, err
    }
    request.SetBasicAuth(provider.user, provider.password)
    request.Header.Set("User-Agent", dynDNS2UserAgent)

    provider.updater.logf(
        "Submitting DynDNS v2 request: provider=%s hostname=%s address=%s",
        provider.providerName,
        hostname,
        myIP,
    )
    response, err := provider.httpClient.Do(request)
    if err != nil {
        return dynDNS2Outcome{}, err
    }
    defer response.Body.Close()

    body, err := io.ReadAll(io.LimitReader(response.Body, dynDNS2ResponseLimit+1))
    if err != nil {
        return dynDNS2Outcome{}, err
    }
    if len(body) > dynDNS2ResponseLimit {
        return dynDNS2Outcome{scope: dynDNS2DisableHostname, message: "response exceeds 4096-byte limit"}, nil
    }
    responseBody := formatUDRResponse(body, provider.user, provider.password)
    provider.updater.logf(
        "DynDNS v2 response: provider=%s hostname=%s status=%s body=%q",
        provider.providerName,
        hostname,
        response.Status,
        responseBody,
    )

    if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
        return dynDNS2Outcome{scope: dynDNS2DisableProvider, message: "HTTP " + response.Status}, nil
    }
    if response.StatusCode >= http.StatusInternalServerError {
        return dynDNS2Outcome{}, fmt.Errorf("HTTP %s: %s", response.Status, responseBody)
    }
    if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
        return dynDNS2Outcome{scope: dynDNS2DisableHostname, message: "HTTP " + response.Status}, nil
    }

    return parseDynDNS2Response(string(body)), nil
}

// parseDynDNS2Response maps the first protocol token to retry state. Because a
// request contains one hostname, receiving multiple result lines is malformed.
func parseDynDNS2Response(body string) dynDNS2Outcome {
    lines := strings.Split(strings.TrimSpace(body), "\n")
    if len(lines) != 1 {
        return dynDNS2Outcome{scope: dynDNS2DisableHostname, message: "malformed multi-line response"}
    }
    fields := strings.Fields(strings.TrimSpace(lines[0]))
    if len(fields) == 0 {
        return dynDNS2Outcome{scope: dynDNS2DisableHostname, message: "empty response"}
    }
    code := strings.ToLower(fields[0])
    switch code {
    case "good", "nochg":
        return dynDNS2Outcome{scope: dynDNS2Success, message: code}
    case "badauth", "!donator", "badagent":
        return dynDNS2Outcome{scope: dynDNS2DisableProvider, message: code}
    case "notfqdn", "nohost", "numhost", "abuse":
        return dynDNS2Outcome{scope: dynDNS2DisableHostname, message: code}
    case "911", "dnserr":
        return dynDNS2Outcome{scope: dynDNS2Temporary, message: code}
    default:
        return dynDNS2Outcome{scope: dynDNS2DisableHostname, message: "unknown response code " + code}
    }
}

// newRuntimeProviders converts validated configuration into stateful provider
// implementations. Legacy configuration becomes one in-memory UDR provider;
// the source configuration file is never changed.
func newRuntimeProviders(config Config, u updater) ([]runtimeProvider, error) {
    if len(config.Providers) == 0 {
        return []runtimeProvider{&udrProvider{
            providerName: "legacy-udr",
            config:       config,
            updater:      u,
        }}, nil
    }

    providers := make([]runtimeProvider, 0, len(config.Providers))
    for _, configured := range config.Providers {
        switch configured.Type {
        case providerTypeUDR:
            providers = append(providers, &udrProvider{
                providerName: configured.Name,
                config: Config{
                    User:     configured.User,
                    Password: configured.Password,
                    Domains:  configured.Domains,
                },
                updater: u,
            })
        case providerTypeDynDNS2:
            endpoint, err := url.Parse(configured.UpdateURL)
            if err != nil {
                return nil, fmt.Errorf("parse provider %s updateUrl: %w", configured.Name, err)
            }
            family, err := configured.normalizedAddressFamily()
            if err != nil {
                return nil, fmt.Errorf("provider %s: %w", configured.Name, err)
            }
            targets := make([]dynDNS2Target, 0, len(configured.Hostnames))
            for _, hostname := range configured.Hostnames {
                targets = append(targets, dynDNS2Target{hostname: normalizeDNSName(hostname)})
            }
            // Do not follow redirects: credentials must never be forwarded to
            // a different host or downgraded to an insecure scheme.
            client := *u.httpClient
            client.CheckRedirect = func(*http.Request, []*http.Request) error {
                return http.ErrUseLastResponse
            }
            providers = append(providers, &dynDNS2Provider{
                providerName:  configured.Name,
                endpoint:      endpoint,
                user:          configured.User,
                password:      configured.Password,
                addressFamily: family,
                targets:       targets,
                httpClient:    &client,
                updater:       u,
                now:           time.Now,
            })
        }
    }
    return providers, nil
}
