package main

import (
    "bytes"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "net/url"
    "strings"
)

const (
    // Provider type identifiers are part of the public JSON configuration.
    providerTypeUDR     = "udr"
    providerTypeDynDNS2 = "dyndns2"

    // Address-family selectors control which discovered addresses are sent to
    // a DynDNS v2 endpoint.
    addressFamilyIPv4 = "ipv4"
    addressFamilyIPv6 = "ipv6"
    addressFamilyBoth = "both"
)

// ProviderConfig is one entry in the canonical multi-provider configuration.
// Fields are flattened in JSON and validated according to Type.
type ProviderConfig struct {
    // Name identifies the provider in logs and must be unique.
    Name          string
    // Type selects the provider implementation ("udr" or "dyndns2").
    Type          string
    // User and Password are provider-specific API credentials.
    User          string
    Password      string
    // Domains is used only by UDR providers.
    Domains       []DomainConfig
    // UpdateURL, AddressFamily, and Hostnames are used only by DynDNS v2
    // providers.
    UpdateURL     string
    AddressFamily string
    Hostnames     []string

    // Presence flags distinguish an omitted type-specific field from a field
    // explicitly supplied as an empty value. This lets validation reject, for
    // example, "hostnames": [] on a UDR provider.
    domainsSet       bool
    updateURLSet     bool
    addressFamilySet bool
    hostnamesSet     bool
}

// UnmarshalJSON keeps provider entries strict even though their fields depend
// on the provider type.
func (provider *ProviderConfig) UnmarshalJSON(data []byte) error {
    type wireProvider struct {
        Name          string         `json:"name"`
        Type          string         `json:"type"`
        User          string         `json:"user"`
        Password      string         `json:"password"`
        Domains       []DomainConfig `json:"domains"`
        UpdateURL     string         `json:"updateUrl"`
        AddressFamily string         `json:"addressFamily"`
        Hostnames     []string       `json:"hostnames"`
    }

    decoder := json.NewDecoder(bytes.NewReader(data))
    decoder.DisallowUnknownFields()
    var wire wireProvider
    if err := decoder.Decode(&wire); err != nil {
        return err
    }
    if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
        if err == nil {
            err = errors.New("multiple JSON values are not allowed")
        }
        return err
    }

    var fields map[string]json.RawMessage
    if err := json.Unmarshal(data, &fields); err != nil {
        return err
    }

    *provider = ProviderConfig{
        Name:             wire.Name,
        Type:             wire.Type,
        User:             wire.User,
        Password:         wire.Password,
        Domains:          wire.Domains,
        UpdateURL:        wire.UpdateURL,
        AddressFamily:    wire.AddressFamily,
        Hostnames:        wire.Hostnames,
        domainsSet:       fields["domains"] != nil,
        updateURLSet:     fields["updateUrl"] != nil,
        addressFamilySet: fields["addressFamily"] != nil,
        hostnamesSet:     fields["hostnames"] != nil,
    }
    return nil
}

// validate enforces common requirements and the schema selected by Type.
func (provider ProviderConfig) validate() error {
    if strings.TrimSpace(provider.Name) == "" {
        return errors.New("name is required")
    }
    if strings.TrimSpace(provider.User) == "" {
        return errors.New("user is required")
    }
    if strings.TrimSpace(provider.Password) == "" {
        return errors.New("password is required")
    }

    switch provider.Type {
    case providerTypeUDR:
        if provider.updateURLSet || provider.addressFamilySet || provider.hostnamesSet ||
            provider.UpdateURL != "" || provider.AddressFamily != "" || provider.Hostnames != nil {
            return errors.New("updateUrl, addressFamily, and hostnames are not valid for an udr provider")
        }
        return validateDomains(provider.Domains, "domains")

    case providerTypeDynDNS2:
        if provider.domainsSet || provider.Domains != nil {
            return errors.New("domains is not valid for a dyndns2 provider")
        }
        endpoint, err := url.Parse(provider.UpdateURL)
        if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
            return fmt.Errorf("updateUrl must be an absolute HTTPS URL")
        }
        if endpoint.User != nil {
            return errors.New("updateUrl must not contain credentials")
        }
        if endpoint.Fragment != "" {
            return errors.New("updateUrl must not contain a fragment")
        }
        if _, err := provider.normalizedAddressFamily(); err != nil {
            return err
        }
        if len(provider.Hostnames) == 0 {
            return errors.New("hostnames must contain at least one entry")
        }
        seen := make(map[string]struct{}, len(provider.Hostnames))
        for i, hostname := range provider.Hostnames {
            name := normalizeDNSName(hostname)
            if name == "" || !strings.Contains(name, ".") {
                return fmt.Errorf("hostnames[%d] must be a fully qualified domain name", i)
            }
            if _, exists := seen[name]; exists {
                return fmt.Errorf("hostname %q is configured more than once", name)
            }
            seen[name] = struct{}{}
        }
        return nil

    default:
        return fmt.Errorf("unsupported provider type %q", provider.Type)
    }
}

// normalizedAddressFamily returns the configured family in canonical form and
// applies the dual-stack default when the field is omitted.
func (provider ProviderConfig) normalizedAddressFamily() (string, error) {
    family := strings.ToLower(strings.TrimSpace(provider.AddressFamily))
    if family == "" {
        return addressFamilyBoth, nil
    }
    switch family {
    case addressFamilyIPv4, addressFamilyIPv6, addressFamilyBoth:
        return family, nil
    default:
        return "", fmt.Errorf("unsupported addressFamily %q", provider.AddressFamily)
    }
}
