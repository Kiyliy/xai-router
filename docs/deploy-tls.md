# Deploying with HTTPS (Caddy)

xai-router speaks plain HTTP. Put it behind a TLS-terminating reverse proxy
for anything reachable from the internet. [Caddy](https://caddyserver.com)
obtains and renews Let's Encrypt certificates automatically, and handles
streaming (SSE) and WebSocket traffic without extra configuration.

No domain? Services such as [sslip.io](https://sslip.io) resolve
`203-0-113-10.sslip.io` to `203.0.113.10`, which is enough for a certificate.

## Option A — everything in Docker Compose

`docker-compose.yml`:

```yaml
services:
  xai-router:
    image: ghcr.io/kiyliy/xai-router:latest
    restart: unless-stopped
    environment:
      XR_LOG_FORMAT: json
    volumes:
      - xai-router-data:/data
    # Not published: only Caddy can reach it.
    expose:
      - "8080"

  caddy:
    image: caddy:2
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
      - "443:443/udp" # HTTP/3
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - caddy-config:/config
    depends_on:
      - xai-router

volumes:
  xai-router-data:
  caddy-data:
  caddy-config:
```

`Caddyfile` (replace the host name):

```caddyfile
gateway.example.com {
	reverse_proxy xai-router:8080 {
		# Forward every SSE chunk immediately.
		flush_interval -1
	}
}
```

Start it and read the generated admin password:

```bash
docker compose up -d
docker compose logs xai-router | grep -A3 "generated an admin password"
```

Open `https://gateway.example.com`.

## Option B — Caddy already runs on the host

Bind the gateway to localhost only and add a site block to the host's
Caddyfile:

```yaml
# docker-compose.override.yml
services:
  xai-router:
    ports: !override
      - "127.0.0.1:8080:8080"
```

```caddyfile
gateway.example.com {
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1
	}
}
```

```bash
caddy validate --config /etc/caddy/Caddyfile && systemctl reload caddy
```

## Client IP addresses

Behind a proxy the gateway sees the proxy's address. It uses
`X-Forwarded-For` only when the direct peer is in **Settings → Security →
Trusted proxies** (CIDRs). The defaults cover loopback and private networks
(Docker bridges included), which fits both options above. Remove them if the
gateway is directly reachable from untrusted private networks.

## Notes

- Do not enable response compression (`encode`) for the gateway's site: it can
  delay SSE chunks.
- WebSocket endpoints (`/v1/realtime`, streaming `/v1/tts` and `/v1/stt`) work
  through Caddy unchanged; clients connect to `wss://gateway.example.com/v1/...`.
