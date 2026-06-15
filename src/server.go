// Package main provides a dynamic DNS client for the United Domains
// Reselling API.
//
// The client periodically discovers the host's public IPv4 and IPv6
// addresses. When either address changes, it updates the configured hostnames
// without modifying unrelated records in their DNS zones.
package main

import (
	"bufio"
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
	maxResponseBodySize = 64 * 1024
	maxZoneResponseSize = 8 * 1024 * 1024
	maxLoggedBodySize   = 4 * 1024
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
	// receives records for the available address families.
	Subdomains []string `json:"subdomains"`
}

// updater coordinates public-address discovery and DNS zone updates.
type updater struct {
	apiURL     string
	ipv4URL    string
	ipv6URL    string
	httpClient *http.Client
	logger     *log.Logger
}

type apiResponse struct {
	Code        string
	Description string
	Properties  map[string][]string
}

type dnsRecord struct {
	Raw   string
	Name  string
	TTL   string
	Type  string
	Value string
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
		logger: log.Default(),
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

	u.logConfiguredZones(ctx, config)

	var lastIPv4 string
	var lastIPv6 string

	for {
		ipv4, ipv6, err := u.externalIPs(ctx)
		if err != nil {
			u.logf("Unable to determine external IP addresses: %v", err)
		} else if ipv4 != lastIPv4 || ipv6 != lastIPv6 {
			u.logf(
				"External IP address change detected: IPv4 %s -> %s, IPv6 %s -> %s",
				displayIPAddress(lastIPv4),
				displayIPAddress(ipv4),
				displayIPAddress(lastIPv6),
				displayIPAddress(ipv6),
			)

			if err := u.updateAllDomains(ctx, config, ipv4, ipv6); err != nil {
				u.logf("DNS update failed: %v", err)
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

// logf writes through the updater's logger when one is configured and falls
// back to the process-wide logger for callers that construct updater directly.
func (u updater) logf(format string, arguments ...any) {
	if u.logger != nil {
		u.logger.Printf(format, arguments...)
		return
	}
	log.Printf(format, arguments...)
}

func (u updater) logConfiguredZones(ctx context.Context, config Config) {
	u.logf("Configured DNS zone entries at startup:")
	for _, domain := range config.Domains {
		records, err := u.queryDNSZoneRecords(ctx, config, domain)
		if err != nil {
			u.logf("\tDNS zone %s: ERR (%v)", normalizeDNSName(domain.Name), err)
			continue
		}

		u.logf("\tDNS zone %s:", normalizeDNSName(domain.Name))
		for _, record := range records {
			u.logf("\t\t%s", record.Raw)
		}
	}
}

func displayIPAddress(address string) string {
	if address == "" {
		return "unavailable"
	}
	return address
}

// externalIPs retrieves and validates both public address families. A lookup
// failure is tolerated when the other address family is available.
func (u updater) externalIPs(ctx context.Context) (string, string, error) {
	ipv4, ipv4Err := u.getIP(ctx, u.ipv4URL, false)
	if ipv4Err != nil {
		u.logf("Unable to determine external IPv4 address: %v", ipv4Err)
	}

	ipv6, ipv6Err := u.getIP(ctx, u.ipv6URL, true)
	if ipv6Err != nil {
		u.logf("Unable to determine external IPv6 address: %v", ipv6Err)
	}

	if ipv4Err != nil && ipv6Err != nil {
		return "", "", errors.Join(
			fmt.Errorf("get IPv4 address: %w", ipv4Err),
			fmt.Errorf("get IPv6 address: %w", ipv6Err),
		)
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

// updateAllDomains updates every configured DNS zone. It attempts every zone
// and joins any errors for the caller.
func (u updater) updateAllDomains(ctx context.Context, config Config, ipv4, ipv6 string) error {
	var updateErrors []error
	for _, domain := range config.Domains {
		if err := u.updateDomain(ctx, config, domain, ipv4, ipv6); err != nil {
			updateErrors = append(updateErrors, err)
		}
	}
	return errors.Join(updateErrors...)
}

// updateDomain updates only the A and AAAA records for configured subdomains,
// then queries the online zone again to verify the changes.
func (u updater) updateDomain(
	ctx context.Context,
	config Config,
	domain DomainConfig,
	ipv4 string,
	ipv6 string,
) error {
	u.logf("Updating DNS zone %s...", normalizeDNSName(domain.Name))

	records, err := u.queryDNSZoneRecords(ctx, config, domain)
	if err != nil {
		return fmt.Errorf("query DNS zone %s before update: %w", domain.Name, err)
	}

	var updateErrors []error
	updated := false
	for _, subdomain := range domain.Subdomains {
		name := normalizeDNSName(subdomain)
		values := buildSubdomainUpdateForm(config, domain, subdomain, records, ipv4, ipv6)
		if !hasRecordChanges(values) {
			u.logf(
				"\tUpdating subdomain %s to IPv4=%s IPv6=%s... OK (already current)",
				name,
				displayIPAddress(ipv4),
				displayIPAddress(ipv6),
			)
			continue
		}

		if _, err := u.callAPI(ctx, values); err != nil {
			updateErr := fmt.Errorf("update subdomain %s: %w", name, err)
			updateErrors = append(updateErrors, updateErr)
			u.logf(
				"\tUpdating subdomain %s to IPv4=%s IPv6=%s... ERR (%v)",
				name,
				displayIPAddress(ipv4),
				displayIPAddress(ipv6),
				err,
			)
			continue
		}
		updated = true
		u.logf(
			"\tUpdating subdomain %s to IPv4=%s IPv6=%s... OK",
			name,
			displayIPAddress(ipv4),
			displayIPAddress(ipv6),
		)
	}

	if err := errors.Join(updateErrors...); err != nil {
		return err
	}

	onlineRecords := records
	if updated {
		onlineRecords, err = u.queryDNSZoneRecords(ctx, config, domain)
		if err != nil {
			return fmt.Errorf("query DNS zone %s after update: %w", domain.Name, err)
		}
	}

	var verificationErrors []error
	for _, subdomain := range domain.Subdomains {
		err := verifySubdomainRecords(subdomain, onlineRecords, ipv4, ipv6)
		name := normalizeDNSName(subdomain)
		if err != nil {
			verificationErrors = append(verificationErrors, err)
			u.logf("\tVerifying subdomain %s online... ERR (%v)", name, err)
			continue
		}
		u.logf("\tVerifying subdomain %s online... OK", name)
	}
	return errors.Join(verificationErrors...)
}

func (u updater) queryDNSZoneRecords(
	ctx context.Context,
	config Config,
	domain DomainConfig,
) ([]dnsRecord, error) {
	response, err := u.callAPI(ctx, url.Values{
		"s_login": {config.User},
		"s_pw":    {config.Password},
		"command": {"QueryDNSZoneRRList"},
		"dnszone": {absoluteDNSName(domain.Name)},
	})
	if err != nil {
		return nil, err
	}

	rawRecords := response.Properties["rr"]
	records := make([]dnsRecord, 0, len(rawRecords))
	for _, rawRecord := range rawRecords {
		record, err := parseDNSRecord(rawRecord)
		if err != nil {
			return nil, fmt.Errorf("parse DNS record %q: %w", rawRecord, err)
		}
		records = append(records, record)
	}
	return records, nil
}

func buildSubdomainUpdateForm(
	config Config,
	domain DomainConfig,
	subdomain string,
	records []dnsRecord,
	ipv4 string,
	ipv6 string,
) url.Values {
	values := url.Values{
		"s_login": {config.User},
		"s_pw":    {config.Password},
		"command": {"UpdateDNSZone"},
		"dnszone": {absoluteDNSName(domain.Name)},
	}

	if verifySubdomainRecords(subdomain, records, ipv4, ipv6) == nil {
		return values
	}

	name := normalizeDNSName(subdomain)
	deleteIndex := 0
	for _, record := range records {
		if record.Name == name && (record.Type == "A" || record.Type == "AAAA") {
			values.Set(fmt.Sprintf("delrr%d", deleteIndex), record.Raw)
			deleteIndex++
		}
	}

	addIndex := 0
	if ipv4 != "" {
		values.Set(
			fmt.Sprintf("addrr%d", addIndex),
			fmt.Sprintf("%s 600 IN A %s", absoluteDNSName(subdomain), ipv4),
		)
		addIndex++
	}
	if ipv6 != "" {
		values.Set(
			fmt.Sprintf("addrr%d", addIndex),
			fmt.Sprintf("%s 600 IN AAAA %s", absoluteDNSName(subdomain), ipv6),
		)
	}
	return values
}

func hasRecordChanges(values url.Values) bool {
	for key := range values {
		if strings.HasPrefix(key, "addrr") || strings.HasPrefix(key, "delrr") {
			return true
		}
	}
	return false
}

func verifySubdomainRecords(subdomain string, records []dnsRecord, ipv4, ipv6 string) error {
	name := normalizeDNSName(subdomain)
	want := map[string]string{}
	if ipv4 != "" {
		want["A"] = ipv4
	}
	if ipv6 != "" {
		want["AAAA"] = ipv6
	}

	found := make(map[string]int)
	for _, record := range records {
		if record.Name != name || (record.Type != "A" && record.Type != "AAAA") {
			continue
		}

		wantValue, exists := want[record.Type]
		if !exists {
			return fmt.Errorf("unexpected %s record %q remains online", record.Type, record.Raw)
		}
		if !ipAddressesEqual(record.Value, wantValue) {
			return fmt.Errorf("%s record is %s, want %s", record.Type, record.Value, wantValue)
		}
		if record.TTL != "600" {
			return fmt.Errorf("%s record TTL is %s, want 600", record.Type, record.TTL)
		}
		found[record.Type]++
	}

	for recordType := range want {
		if found[recordType] != 1 {
			return fmt.Errorf(
				"found %d matching %s records, want exactly 1",
				found[recordType],
				recordType,
			)
		}
	}
	return nil
}

func ipAddressesEqual(first, second string) bool {
	firstIP := net.ParseIP(first)
	secondIP := net.ParseIP(second)
	return firstIP != nil && secondIP != nil && firstIP.Equal(secondIP)
}

func parseDNSRecord(raw string) (dnsRecord, error) {
	fields := strings.Fields(raw)
	if len(fields) < 4 {
		return dnsRecord{}, errors.New("record has fewer than four fields")
	}

	inIndex := -1
	for i, field := range fields {
		if strings.EqualFold(field, "IN") {
			inIndex = i
			break
		}
	}
	if inIndex < 1 || inIndex+2 >= len(fields) {
		return dnsRecord{}, errors.New("record does not contain an IN class, type, and value")
	}

	ttl := ""
	if inIndex > 1 {
		ttl = fields[inIndex-1]
	}
	return dnsRecord{
		Raw:   strings.TrimSpace(raw),
		Name:  normalizeDNSName(fields[0]),
		TTL:   ttl,
		Type:  strings.ToUpper(fields[inIndex+1]),
		Value: strings.Join(fields[inIndex+2:], " "),
	}, nil
}

func (u updater) callAPI(ctx context.Context, values url.Values) (apiResponse, error) {
	command := values.Get("command")
	zone := normalizeDNSName(values.Get("dnszone"))
	u.logf("Submitting UDR API request: command=%s zone=%s", command, zone)
	for _, change := range recordChanges(values) {
		u.logf("UDR DNS change: zone=%s %s=%q", zone, change.key, change.value)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		u.apiURL,
		strings.NewReader(values.Encode()),
	)
	if err != nil {
		updateErr := fmt.Errorf("create %s request: %w", command, err)
		u.logf("UDR API request failed: command=%s zone=%s error=%v", command, zone, updateErr)
		return apiResponse{}, updateErr
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := u.httpClient.Do(request)
	if err != nil {
		updateErr := fmt.Errorf("%s request: %w", command, err)
		u.logf("UDR API request failed: command=%s zone=%s error=%v", command, zone, updateErr)
		return apiResponse{}, updateErr
	}
	defer response.Body.Close()

	responseLimit := responseBodyLimit(command)
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(responseLimit)+1))
	if err != nil {
		updateErr := fmt.Errorf("read %s response: %w", command, err)
		u.logf(
			"UDR API request failed: command=%s zone=%s status=%s error=%v",
			command,
			zone,
			response.Status,
			updateErr,
		)
		return apiResponse{}, updateErr
	}
	if len(body) > responseLimit {
		updateErr := fmt.Errorf(
			"read %s response: response exceeds %d-byte limit",
			command,
			responseLimit,
		)
		u.logf(
			"UDR API request failed: command=%s zone=%s status=%s error=%v",
			command,
			zone,
			response.Status,
			updateErr,
		)
		return apiResponse{}, updateErr
	}
	responseBody := formatUDRResponse(body, values.Get("s_login"), values.Get("s_pw"))
	u.logf(
		"UDR API response: command=%s zone=%s status=%s body=%q",
		command,
		zone,
		response.Status,
		responseBody,
	)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		updateErr := fmt.Errorf(
			"%s: HTTP %s: %s",
			command,
			response.Status,
			responseBody,
		)
		u.logf("UDR API request failed: command=%s zone=%s error=%v", command, zone, updateErr)
		return apiResponse{}, updateErr
	}

	apiResult, err := parseAPIResponse(string(body))
	if err != nil {
		updateErr := fmt.Errorf("parse %s response: %w", command, err)
		u.logf("UDR API request failed: command=%s zone=%s error=%v", command, zone, updateErr)
		return apiResponse{}, updateErr
	}
	if apiResult.Code != "200" {
		updateErr := fmt.Errorf(
			"%s: API code %s: %s",
			command,
			apiResult.Code,
			apiResult.Description,
		)
		u.logf("UDR API request failed: command=%s zone=%s error=%v", command, zone, updateErr)
		return apiResponse{}, updateErr
	}
	u.logf("UDR API request completed: command=%s zone=%s", command, zone)
	return apiResult, nil
}

func responseBodyLimit(command string) int {
	if command == "QueryDNSZoneRRList" {
		return maxZoneResponseSize
	}
	return maxResponseBodySize
}

type recordChange struct {
	key   string
	value string
}

func recordChanges(values url.Values) []recordChange {
	var changes []recordChange
	for _, prefix := range []string{"delrr", "addrr"} {
		for index := 0; ; index++ {
			key := fmt.Sprintf("%s%d", prefix, index)
			value := values.Get(key)
			if value == "" {
				break
			}
			changes = append(changes, recordChange{key: key, value: value})
		}
	}
	return changes
}

// formatUDRResponse converts the provider response to a bounded, single-line
// log value and redacts configured credentials if the API echoes them.
func formatUDRResponse(body []byte, credentials ...string) string {
	response := strings.TrimSpace(string(body))
	response = strings.Join(strings.Fields(response), " ")
	for _, credential := range credentials {
		if credential != "" {
			response = strings.ReplaceAll(response, credential, "[REDACTED]")
		}
	}
	if response == "" {
		return "<empty>"
	}
	if len(response) > maxLoggedBodySize {
		return response[:maxLoggedBodySize] + "...[truncated]"
	}
	return response
}

func parseAPIResponse(body string) (apiResponse, error) {
	response := apiResponse{Properties: make(map[string][]string)}
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, maxResponseBodySize), maxZoneResponseSize)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch strings.ToLower(key) {
		case "code":
			response.Code = value
		case "description":
			response.Description = value
		default:
			if propertyName, ok := apiPropertyName(key); ok {
				response.Properties[propertyName] = append(response.Properties[propertyName], value)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return apiResponse{}, err
	}
	if response.Code == "" {
		return apiResponse{}, errors.New("response code is missing")
	}
	return response, nil
}

func apiPropertyName(key string) (string, bool) {
	lowerKey := strings.ToLower(strings.TrimSpace(key))
	const prefix = "property["
	if !strings.HasPrefix(lowerKey, prefix) {
		return "", false
	}
	end := strings.Index(lowerKey[len(prefix):], "]")
	if end < 0 {
		return "", false
	}
	return lowerKey[len(prefix) : len(prefix)+end], true
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
