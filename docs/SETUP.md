# Setup Guide — Project XK

---

## Prerequisites

| Requirement | Version | Notes |
|---|---|---|
| Linux | x86_64 or arm64 | Debian 11+, Ubuntu 20.04+, RHEL 8+ |
| Go | 1.22+ | For building from source |
| Node.js | 20+ | For building web UI |
| pnpm | 9+ | `npm install -g pnpm` |
| Caddy | Latest | Reverse proxy (auto-HTTPS) |
| ktalk.ru account | Free tier | Room must exist before deployment |

---

## Quick Install (recommended)

```bash
curl -fsSL https://github.com/cwash797-cmd/Project-XK/releases/latest/download/install.sh | sudo bash
```

The installer will:
1. Detect your package manager (apt/dnf/yum)
2. Install Caddy from the official repository
3. Download `ktalk-core` and `ktalk-panel` binaries
4. Create system user `xk`
5. Write default config to `/etc/ktalk-panel/config.json`
6. Install and enable the systemd service

After installation, continue with [Initial Configuration](#initial-configuration).

---

## Package Install (.deb / .rpm)

### Debian / Ubuntu

```bash
# Download the .deb for your architecture
curl -LO https://github.com/cwash797-cmd/Project-XK/releases/latest/download/ktalk-panel_amd64.deb

# Install
sudo dpkg -i ktalk-panel_amd64.deb

# Or for arm64:
# curl -LO .../ktalk-panel_arm64.deb && sudo dpkg -i ktalk-panel_arm64.deb
```

### RHEL / CentOS / Fedora

```bash
curl -LO https://github.com/cwash797-cmd/Project-XK/releases/latest/download/ktalk-panel_amd64.rpm
sudo rpm -i ktalk-panel_amd64.rpm
```

---

## Docker

```bash
docker run -d \
  --name ktalk-panel \
  -p 8888:8888 \
  -v /etc/ktalk-panel:/etc/ktalk-panel \
  ghcr.io/cwash797-cmd/project-xk:latest
```

### Docker Compose

```yaml
version: "3.9"
services:
  ktalk-panel:
    image: ghcr.io/cwash797-cmd/project-xk:latest
    restart: unless-stopped
    ports:
      - "8888:8888"
    volumes:
      - ktalk-config:/etc/ktalk-panel

volumes:
  ktalk-config:
```

---

## Build from Source

```bash
git clone https://github.com/cwash797-cmd/Project-XK.git
cd Project-XK

# Build everything (web UI + Go binaries)
make build

# Run tests
make test

# Build .deb / .rpm packages (requires nfpm)
make package

# Build Docker image
make docker
```

### Available Make Targets

```
build           Build ktalk-core and ktalk-panel (with embedded web UI)
test            Run all Go tests with -race flag
fmt             Run gofmt on all Go files
vet             Run go vet on all Go files
clean           Remove build artifacts
package         Build .deb and .rpm packages (amd64)
package-arm64   Build .deb and .rpm packages (arm64)
docker          Build Docker image
docker-push     Build and push Docker image to GHCR
```

---

## Initial Configuration

### 1. Create a ktalk.ru room

Before configuring Project XK, you need a ktalk.ru room:

1. Go to [ktalk.ru](https://ktalk.ru) and create a free account.
2. Create a room and note the room URL: `https://ktalk.ru/cb140blkff7i`
3. Note the room ID from the URL (e.g., `cb140blkff7i`).

### 2. Edit the config file

Config location: `/etc/ktalk-panel/config.json`

```json
{
  "listen_addr": "0.0.0.0:8888",
  "admin_password_hash": "",
  "clients": []
}
```

### 3. Set admin password

On first start the panel will redirect to the setup page at `http://localhost:8888/setup`.

Or set it via CLI:

```bash
# Generate bcrypt hash
htpasswd -bnBC 12 "" 'YourPassword' | tr -d ':\n'

# Paste the hash into config.json → "admin_password_hash"
```

### 4. Start the service

```bash
sudo systemctl start ktalk-panel
sudo systemctl status ktalk-panel

# View logs
sudo journalctl -u ktalk-panel -f
```

### 5. Open the admin panel

Navigate to `http://<your-server-ip>:8888` (or your domain if Caddy is configured).

---

## Caddy Configuration (HTTPS)

Create `/etc/caddy/Caddyfile`:

```caddyfile
your-domain.example.com {
    reverse_proxy localhost:8888
}
```

Reload Caddy:
```bash
sudo systemctl reload caddy
```

Caddy will automatically obtain and renew a Let's Encrypt TLS certificate.

---

## Creating a Tunnel Client

### Via Web UI

1. Log into the admin panel at `https://your-domain.example.com`
2. Click **"New Client"**
3. Fill in:
   - **Room URL:** `https://ktalk.ru/cb140blkff7i`
   - **Label:** any descriptive name
4. Click **Create** — the panel generates a key and starts the tunnel.

### Via API

```bash
# Create a new tunnel client
curl -X POST https://your-domain.example.com/api/clients \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d '{
    "room_url": "https://ktalk.ru/cb140blkff7i",
    "label": "my-tunnel"
  }'

# Response:
# {
#   "id": "abc123",
#   "socks5_port": 1080,
#   "key": "deadbeef..."
# }
```

---

## Using the Tunnel (SOCKS5)

Once a tunnel is active, its SOCKS5 proxy is available at `127.0.0.1:<port>`.

### curl

```bash
curl --socks5 127.0.0.1:1080 http://internal-service.local/api/status
```

### SSH ProxyCommand

```bash
ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' user@remote-host
```

### Browser (Firefox)

Settings → Network → Manual proxy → SOCKS Host: `127.0.0.1`, Port: `1080`, SOCKS5.

### System-wide (Linux)

```bash
# /etc/environment or ~/.profile
export all_proxy=socks5://127.0.0.1:1080
export ALL_PROXY=socks5://127.0.0.1:1080
```

---

## Monitoring

### SSE Event Stream

```bash
# Watch real-time tunnel events
curl -N http://localhost:8888/api/events \
  -H "Authorization: Bearer <token>"

# Events are JSON:
# {"type":"state","data":{"id":"abc","status":"active","bytes_in":1234,"bytes_out":5678}}
# {"type":"log","data":"[abc] ICE connected"}
# {"type":"ping","data":"2026-05-19T12:00:00Z"}
```

### Health Check

```bash
curl http://localhost:8888/health
# {"status":"ok","uptime":"2h15m30s"}
```

### Prometheus Metrics

```bash
curl http://localhost:8888/metrics
```

Configure Prometheus to scrape `http://localhost:8888/metrics`.

---

## Troubleshooting

### Tunnel won't connect

```bash
# Check XMPP WebSocket is reachable
curl -v "https://ilte0310.ktalk.ru/api/rooms/<room-id>" \
  -H "Authorization: Session <your-token>"

# Expected: 200 OK with JSON body

# Check panel logs
sudo journalctl -u ktalk-panel -n 100 --no-pager
```

### ICE connection fails

```bash
# Verify STUN/TURN servers are reachable
# (room config is fetched from /api/rooms/<id>)
# Check firewall: UDP 3478, UDP 10000-60000 must be open outbound
sudo ufw status
```

### Session expires unexpectedly (free tier)

On free-tier ktalk.ru rooms, there may be a call duration limit (~40 minutes).  
The panel's watchdog performs a preventive reconnect every 35 minutes.

To adjust the watchdog interval:

```json
{
  "watchdog_interval_minutes": 35
}
```

### Shard change disconnect

If you see `shard changed` in logs, ktalk.ru rebalanced your connection to a new server.  
This is handled automatically — the client will reconnect within 5 seconds.

---

## Upgrading

### From binary

```bash
# Download new install script / package and re-run
curl -fsSL https://github.com/.../install.sh | sudo bash
```

### From source

```bash
git pull
make build
sudo systemctl restart ktalk-panel
```

---

## Uninstall

```bash
# Stop and disable service
sudo systemctl stop ktalk-panel
sudo systemctl disable ktalk-panel

# Remove package
sudo dpkg -r ktalk-panel          # Debian/Ubuntu
sudo rpm -e ktalk-panel            # RHEL

# Remove config (optional)
sudo rm -rf /etc/ktalk-panel /var/lib/ktalk-panel

# Remove user
sudo userdel xk
```
