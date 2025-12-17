# x-ui-sni-router

Layer‑4 SNI-based TCP router for x-ui.  
A small Go daemon that inspects the TLS SNI during the handshake and routes incoming TCP connections to vless‑reality instances by matching SNI to inbounds defined in the x‑ui SQLite database.
With that router you can use 1 server for hosting multiples vless-reality servers. Actual for Russia-specific censorship.

## Visual block-scheme
          ┌──────────────────────┐
          │      CLIENTS         │
          │                      │
          │  SNI = a.example.com │
          │  SNI = b.example.com │
          │  SNI = c.example.com │
          └─────────┬────────────┘
                    │
                    │ TCP :443
                    ▼
        ┌───────────────────────────────┐
        │        PUBLIC IP              │
        │        66.xxx.xxx.xx          │
        │                               │
        │   x-ui-sni-router (Go)        │
        │   ────────────────────────    │
        │   • accept(:443)              │
        │   • read ClientHello          │
        │   • extract SNI               │
        │   • lookup table              │
        │   • raw TCP proxy             │
        └───────────┬───────┬───────── ─┘
                    │       │
          ┌─────────┘       └─────────┐
          ▼                           ▼
┌───────────────────┐       ┌───────────────────┐
│ xray reality A    │       │ xray reality B    │
│ listen 127.0.0.1  │       │ listen 127.0.0.1  │
│ port 55555        │       │ port 55556        │
│                   │       │                   │
│ SNI=a.example.com │       │ SNI=b.example.com │
└───────────────────┘       └───────────────────┘
                    ▲
                    │
          ┌─────────┴─────────┐
          ▼                   ▼
┌───────────────────┐   ┌───────────────────┐
│ xray reality C    │   │ (other inbound)   │
│ port 55557        │   │                   │
│ SNI=c.example.com │   │                   │
└───────────────────┘   └───────────────────┘



## Highlights
- Route incoming TLS connections by SNI at Layer 4 (no TLS termination).
- Reads inbounds from x‑ui SQLite DB (inbounds → stream_settings → realitySettings.target).
- Periodically refreshes the route table (default: every 30s).
- Small, dependency‑free Go binary suitable for systemd and containers.

## How it works (brief)
1. Reads the inbounds rows from the x‑ui sqlite DB and extracts realitySettings.target.
2. Builds a mapping host → inbound_port.
3. Listens on a public TCP port (default :443).
4. Parses SNI from the TLS ClientHello using a lightweight vhost parser.
5. For matched SNI, proxies the TCP stream to the corresponding local port.

## Requirements
- Go 1.18+ to build from source (optional if you use prebuilt binary).
- Read access to x‑ui SQLite DB (default: /etc/x-ui/x-ui.db).
- Permission to bind the desired listen port (root or CAP_NET_BIND_SERVICE for <1024).

## Build
- Clone and build:
  ```bash
  go build -o x-ui-sni-router ./...
  ```

## Usage
- Default (uses /etc/x-ui/x-ui.db):
  ```bash
  sudo ./x-ui-sni-router
  ```

- Specify a custom DB path:
  ```bash
  ./x-ui-sni-router -db_path ./x-ui.db
  ```

## CLI flags
- `-db_path string`
    Path to x‑ui SQLite DB (default "/etc/x-ui/x-ui.db")

## Systemd unit example
- /etc/systemd/system/x-ui-sni-router.service
  ```ini
  [Unit]
  Description=x-ui SNI Router
  After=network.target

  [Service]
  ExecStart=/usr/local/bin/x-ui-sni-router -db_path /etc/x-ui/x-ui.db
  Restart=on-failure
  User=nobody
  Group=nogroup
  AmbientCapabilities=CAP_NET_BIND_SERVICE

  [Install]
  WantedBy=multi-user.target
  ```

## Networking and ports
- The daemon expects vless‑reality instances to listen on localhost (127.0.0.1:PORT). It proxies at TCP level — no TLS termination, no certificate handling.

## Troubleshooting
- "no route for SNI" — ensure realitySettings.target host matches the SNI hostname and that the inbound exists in the DB.
- Corrupt JSON in stream_settings will be skipped and logged as a warning.
- If binding to :443 fails, run with appropriate privileges or grant the binary CAP_NET_BIND_SERVICE:
  ```bash
  sudo setcap 'cap_net_bind_service=+ep' ./x-ui-sni-router
  ```

## Security notes
- The project only routes by SNI; it does not inspect or modify encrypted payloads.
- Protect access to the x‑ui DB to prevent route tampering.
- Use firewall rules and least privilege for the service user.

## Contributing
- Issues and PRs welcome. For larger changes, open an issue first to discuss design.

## Contact
- Use the repository issue tracker for questions, bugs, or feature requests.
