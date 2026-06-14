// Package main provides a dynamic DNS client for the United Domains
// Reselling API.
//
// The client periodically discovers the host's public IPv4 and IPv6
// addresses. When either address changes, it replaces each configured DNS
// zone with generated A and AAAA records for its configured hostnames.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Default service endpoints and polling behavior. They are kept separate
	// from updater so tests can substitute local HTTP transports and URLs.
	defaultAPIURL       = "https://api.domainreselling.de/api/call.cgi"
	defaultIPv4URL      = "https://ipv4.myexternalip.com/raw"
	defaultIPv6URL      = "https://ipv6.myexternalip.com/raw"
	defaultPollInterval = time.Minute
	defaultServiceName  = "UDResellingDynDNS"
)

// Config contains the reseller credentials and DNS zones managed by the
// application.
type Config struct {
	// User is the United Domains Reselling API login.
	User string `json:"user"`

	// Password is the United Domains Reselling API password.
	Password string `json:"password"`

	// PollInterval is a Go duration such as "30s" or "5m". An empty value
	// uses defaultPollInterval.
	PollInterval string `json:"pollInterval,omitempty"`

	// Domains lists the DNS zones to update when an external address changes.
	Domains []DomainConfig `json:"domains"`
}

// DomainConfig describes one DNS zone and its dynamic hostnames.
type DomainConfig struct {
	// Name is the zone's fully qualified domain name, with or without a
	// trailing dot.
	Name string `json:"name"`

	// Subdomains contains fully qualified names within Name. Each name
	// receives one A and one AAAA record.
	Subdomains []string `json:"subdomains"`
}

// updater coordinates public-address discovery and DNS zone updates.
type updater struct {
	apiURL     string
	ipv4URL    string
	ipv6URL    string
	httpClient *http.Client
}

// main loads configuration and delegates process lifecycle handling to the
// current operating system.
func main() {
	configPath := flag.String("config", "config.json", "path to the JSON configuration file")
	logPath := flag.String("log", "", "optional path to an append-only log file")
	serviceName := flag.String("service-name", defaultServiceName, "Windows service name")
	flag.Parse()

	if err := configureLogging(*logPath); err != nil {
		log.Fatal(err)
	}

	config, err := readConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	pollInterval, err := config.interval()
	if err != nil {
		log.Fatal(err)
	}

	u := updater{
		apiURL:  defaultAPIURL,
		ipv4URL: defaultIPv4URL,
		ipv6URL: defaultIPv6URL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	run := func(ctx context.Context) error {
		return u.run(ctx, config, pollInterval)
	}
	if err := runPlatform(*serviceName, run); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

// configureLogging sends logs to stderr by default or to an append-only file
// when path is set. A file is useful when running under the Windows Service
// Control Manager, which does not preserve console output.
func configureLogging(path string) error {
	if path == "" {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", path, err)
	}
	log.SetOutput(file)
	return nil
}

// readConfig decodes and validates a single JSON configuration value from
// path. Unknown fields and trailing JSON values are rejected to catch
// configuration mistakes at startup.
func readConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()

	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values are not allowed")
		}
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if err := config.validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %q: %w", path, err)
	}

	return config, nil
}

// validate checks required values and prevents ambiguous or invalid zone
// definitions.
func (config Config) validate() error {
	if strings.TrimSpace(config.User) == "" {
		return errors.New("user is required")
	}
	if strings.TrimSpace(config.Password) == "" {
		return errors.New("password is required")
	}
	if len(config.Domains) == 0 {
		return errors.New("at least one domain is required")
	}
	if _, err := config.interval(); err != nil {
		return err
	}

	seenDomains := make(map[string]struct{}, len(config.Domains))
	for i, domain := range config.Domains {
		domainName := normalizeDNSName(domain.Name)
		if domainName == "" {
			return fmt.Errorf("domains[%d].name is required", i)
		}
		if _, exists := seenDomains[domainName]; exists {
			return fmt.Errorf("domain %q is configured more than once", domainName)
		}
		seenDomains[domainName] = struct{}{}

		if len(domain.Subdomains) == 0 {
			return fmt.Errorf("domains[%d].subdomains must contain at least one entry", i)
		}

		seenSubdomains := make(map[string]struct{}, len(domain.Subdomains))
		for j, subdomain := range domain.Subdomains {
			subdomainName := normalizeDNSName(subdomain)
			if subdomainName == "" {
				return fmt.Errorf("domains[%d].subdomains[%d] is empty", i, j)
			}
			if subdomainName != domainName && !strings.HasSuffix(subdomainName, "."+domainName) {
				return fmt.Errorf("subdomain %q is not within domain %q", subdomainName, domainName)
			}
			if _, exists := seenSubdomains[subdomainName]; exists {
				return fmt.Errorf("subdomain %q is configured more than once for domain %q", subdomainName, domainName)
			}
			seenSubdomains[subdomainName] = struct{}{}
		}
	}

	return nil
}

// interval parses PollInterval or returns the default polling interval when
// the setting is omitted.
func (config Config) interval() (time.Duration, error) {
	if config.PollInterval == "" {
		return defaultPollInterval, nil
	}

	interval, err := time.ParseDuration(config.PollInterval)
	if err != nil {
		return 0, fmt.Errorf("invalid pollInterval %q: %w", config.PollInterval, err)
	}
	if interval <= 0 {
		return 0, errors.New("pollInterval must be greater than zero")
	}
	return interval, nil
}

// run polls for public addresses until ctx is canceled. A failed lookup or
// update is logged and retried on the next tick. Cached addresses advance only
// after every configured zone has been updated successfully.
func (u updater) run(ctx context.Context, config Config, pollInterval time.Duration) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastIPv4 string
	var lastIPv6 string

	for {
		ipv4, ipv6, err := u.externalIPs(ctx)
		if err != nil {
			log.Printf("Unable to determine external IP addresses: %v", err)
		} else if ipv4 != lastIPv4 || ipv6 != lastIPv6 {
			log.Printf("External IP address changed: IPv4=%s IPv6=%s", ipv4, ipv6)

			if err := u.updateAllDomains(ctx, config, ipv4, ipv6); err != nil {
				log.Printf("DNS update failed: %v", err)
			} else {
				lastIPv4 = ipv4
				lastIPv6 = ipv6
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// externalIPs retrieves and validates both public address families.
func (u updater) externalIPs(ctx context.Context) (string, string, error) {
	ipv4, err := u.getIP(ctx, u.ipv4URL, false)
	if err != nil {
		return "", "", fmt.Errorf("get IPv4 address: %w", err)
	}

	ipv6, err := u.getIP(ctx, u.ipv6URL, true)
	if err != nil {
		return "", "", fmt.Errorf("get IPv6 address: %w", err)
	}

	return ipv4, ipv6, nil
}

// getIP fetches one address from endpoint and verifies that it belongs to the
// requested address family.
func (u updater) getIP(ctx context.Context, endpoint string, wantIPv6 bool) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}

	response, err := u.httpClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("unexpected HTTP status %s", response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, 256))
	if err != nil {
		return "", err
	}

	address := strings.TrimSpace(string(body))
	ip := net.ParseIP(address)
	if ip == nil {
		return "", fmt.Errorf("invalid IP address %q", address)
	}
	if wantIPv6 && ip.To4() != nil {
		return "", fmt.Errorf("expected IPv6 address, got %q", address)
	}
	if !wantIPv6 && ip.To4() == nil {
		return "", fmt.Errorf("expected IPv4 address, got %q", address)
	}

	return address, nil
}

// updateAllDomains submits one independent API request per configured DNS
// zone. It attempts every zone and joins any errors for the caller.
func (u updater) updateAllDomains(ctx context.Context, config Config, ipv4, ipv6 string) error {
	var updateErrors []error
	for _, domain := range config.Domains {
		if err := u.updateDomain(ctx, config, domain, ipv4, ipv6); err != nil {
			updateErrors = append(updateErrors, err)
		}
	}
	return errors.Join(updateErrors...)
}

// updateDomain replaces one DNS zone with records generated from domain and
// the current public addresses.
func (u updater) updateDomain(
	ctx context.Context,
	config Config,
	domain DomainConfig,
	ipv4 string,
	ipv6 string,
) error {
	values := buildUpdateForm(config, domain, ipv4, ipv6)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.apiURL, strings.NewReader(values.Encode()))
	if err != nil {
		return fmt.Errorf("create update request for %s: %w", domain.Name, err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := u.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("update domain %s: %w", domain.Name, err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("read response for domain %s: %w", domain.Name, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("update domain %s: HTTP %s: %s", domain.Name, response.Status, strings.TrimSpace(string(body)))
	}

	log.Printf("Updated DNS zone %s", normalizeDNSName(domain.Name))
	return nil
}

// buildUpdateForm constructs the form fields expected by UpdateDNSZone.
// Record numbering is contiguous, with one A and one AAAA record for each
// configured subdomain.
func buildUpdateForm(config Config, domain DomainConfig, ipv4, ipv6 string) url.Values {
	domainName := absoluteDNSName(domain.Name)

	values := url.Values{
		"s_login": {config.User},
		"s_pw":    {config.Password},
		"command": {"UpdateDNSZone"},
		"dnszone": {domainName},
	}

	var records []string
	for _, subdomain := range domain.Subdomains {
		name := absoluteDNSName(subdomain)
		records = append(records,
			fmt.Sprintf("%s 600 IN A %s", name, ipv4),
			fmt.Sprintf("%s 600 IN AAAA %s", name, ipv6),
		)
	}

	for i, record := range records {
		values.Set(fmt.Sprintf("rr%d", i), record)
	}
	return values
}

// normalizeDNSName trims whitespace and a trailing dot, then lowercases a DNS
// name for validation and comparison.
func normalizeDNSName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// absoluteDNSName returns a normalized DNS name with a trailing dot.
func absoluteDNSName(name string) string {
	return normalizeDNSName(name) + "."
}
