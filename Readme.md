# United Domains Reselling DynDNS

This application reads the public IPv4 and IPv6 addresses of the host and
updates one or more DNS zones through the United Domains Reselling API.

On startup, and then at the configured interval, it:

1. Fetches the public IPv4 and IPv6 addresses.
2. Detects address changes.
3. Sends one `UpdateDNSZone` request for each configured domain.
4. Creates A, AAAA, and timestamp TXT records for every configured subdomain.

Each configured zone also retains the original application's apex and wildcard
CNAME records.

## Configuration

Configuration is JSON. Copy the example and restrict access because it contains
the reseller password:

```sh
cp config.json.example config.json
chmod 600 config.json
```

```json
{
  "user": "reseller-user",
  "password": "replace-me",
  "pollInterval": "1m",
  "domains": [
    {
      "name": "example.com",
      "cnameMaster": "www.example.com",
      "subdomains": [
        "home.example.com",
        "vpn.example.com"
      ]
    },
    {
      "name": "example.net",
      "cnameMaster": "www.example.net",
      "subdomains": [
        "home.example.net"
      ]
    }
  ]
}
```

`pollInterval` accepts a Go duration such as `30s`, `1m`, or `5m` and defaults
to `1m`. Domain names may include a trailing dot. Every subdomain must be a
fully qualified name within its associated domain.

The old `properties.ini` format is no longer supported.

## Standalone Build

Go 1.22 or newer is required.

```sh
make standalone
./bin/ud-reselling-dyndns -config ./config.json
```

The resulting binary is statically built and placed in `bin/`.

Use `-log /path/to/file.log` to write logs to a file instead of standard
error.

## Windows Server

The application supports Windows Server as either an interactive console
application or a native Windows service. No third-party service wrapper is
required.

Cross-compile the 64-bit Windows binary from Linux or macOS:

```sh
make windows
```

For Windows Server on ARM64:

```sh
make windows-arm64
```

To build directly in PowerShell on Windows Server with Go installed:

```powershell
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags="-s -w" `
  -o bin\windows-amd64\ud-reselling-dyndns.exe .\src
```

Run it interactively:

```powershell
.\bin\windows-amd64\ud-reselling-dyndns.exe -config .\config.json
```

To install it as an automatically started Windows service, open an elevated
Windows PowerShell session from the repository root:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\windows\install-service.ps1
```

The installer:

- Copies the executable to
  `C:\Program Files\UDResellingDynDNS\ud-reselling-dyndns.exe`.
- Copies the configuration to
  `C:\ProgramData\UDResellingDynDNS\config.json`.
- Runs the service as the built-in `LocalService` account.
- Restricts the data directory permissions.
- Writes logs to `C:\ProgramData\UDResellingDynDNS\service.log`.
- Configures automatic startup and restart-on-failure behavior.

After editing the installed configuration, restart the service:

```powershell
Restart-Service UDResellingDynDNS
Get-Content C:\ProgramData\UDResellingDynDNS\service.log -Tail 50
```

Remove the service while retaining its configuration and logs:

```powershell
.\windows\uninstall-service.ps1
```

Pass `-RemoveFiles` to also delete the installed executable, configuration,
and logs.

## Container Build

Docker is the only host dependency for the container build:

```sh
make container
docker run --rm \
  --mount type=bind,source="$(pwd)/config.json",target=/config/config.json,readonly \
  ghcr.io/simoncahill/ud-reselling-dyndns:latest
```

Set `IMAGE` to use another tag:

```sh
make container IMAGE=registry.example.com/ud-reselling-dyndns:latest
```

The image uses an Alpine build stage and a minimal Alpine runtime containing
only CA certificates, a non-root user, and the application binary.

## Standalone systemd Service

Build the application, then install the binary, configuration, and unit:

```sh
sudo useradd --system --home /nonexistent --shell /usr/sbin/nologin ud-reselling-dyndns
sudo install -Dm755 bin/ud-reselling-dyndns /usr/local/bin/ud-reselling-dyndns
sudo install -d -m750 -o root -g ud-reselling-dyndns /etc/ud-reselling-dyndns
sudo install -m640 -o root -g ud-reselling-dyndns config.json /etc/ud-reselling-dyndns/config.json
sudo install -Dm644 systemd/ud-reselling-dyndns.service /etc/systemd/system/ud-reselling-dyndns.service
sudo systemctl daemon-reload
sudo systemctl enable --now ud-reselling-dyndns.service
```

## Container systemd Service

The container unit pulls the configured image whenever it starts, removes any
old container with the same name, and runs with the configuration mounted
read-only:

```sh
sudo install -d -m700 /etc/ud-reselling-dyndns
sudo install -m600 config.json /etc/ud-reselling-dyndns/config.json
sudo install -Dm644 systemd/ud-reselling-dyndns-container.service \
  /etc/systemd/system/ud-reselling-dyndns-container.service
sudo systemctl daemon-reload
sudo systemctl enable --now ud-reselling-dyndns-container.service
```

The unit defaults to
`ghcr.io/simoncahill/ud-reselling-dyndns:latest`. Override it by creating
`/etc/default/ud-reselling-dyndns`:

```sh
DYNDNS_IMAGE=registry.example.com/ud-reselling-dyndns:latest
```

Docker must already be installed and enabled on the host.

## Testing

```sh
make test
```

## Continuous Integration

GitHub Actions builds, tests, and uploads binaries for:

- Windows x86-64 and ARM64.
- Linux x86-64, armhf (`GOARM=7`), and aarch64.
- macOS ARM64.

The container workflow builds Linux images for amd64, arm/v7, and arm64. Every
branch push, version tag, and manual workflow run publishes the image to GitHub
Container Registry under `ghcr.io/simoncahill/ud-reselling-dyndns`. The default
branch also publishes `latest`; version tags such as `v1.2.3` publish semantic
version tags. Registry authentication and publication are mandatory, so either
failure fails the workflow. Pull requests run the binary build and test
workflow, but do not invoke the publishing workflow.

The software is provided without warranty. Use it only after confirming that
the generated records represent the complete desired contents for each managed
DNS zone.
