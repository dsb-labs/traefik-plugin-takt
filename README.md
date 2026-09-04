# traefik-plugin-orca

A [traefik](https://traefik.io) provider plugin that publishes the services an
[orca](https://github.com/dsb-labs/orca) server holds as traefik dynamic
configuration. Traefik then balances requests across the healthy workload
instances each service selects, and follows the fleet as instances scale, fail
their checks, or move ports.

The plugin polls the orca API for services labelled `traefik.enable=true`. Each
one becomes a load-balanced traefik service whose servers are the addresses
orca reports as
[backends](https://github.com/dsb-labs/orca/blob/main/docs/services.md#backends).
Routers are read from the orca service's labels, following the same convention
as traefik's docker provider. A configuration is pushed to traefik only when it
differs from the last one pushed.

## Installation

Name the plugin and its version in traefik's static configuration. Traefik
downloads the tagged source and interprets it at startup:

```yaml
experimental:
  plugins:
    orca:
      moduleName: github.com/dsb-labs/traefik-plugin-orca
      version: v0.1.0

providers:
  plugin:
    orca:
      endpoint: http://127.0.0.1:7373
      pollInterval: 5s
```

To run an unreleased checkout instead, use traefik's
[local plugin](https://plugins.traefik.io/create) mode. Place this repository
at `plugins-local/src/github.com/dsb-labs/traefik-plugin-orca`, relative to
where traefik runs. A symlink works:

```sh
mkdir -p plugins-local/src/github.com/dsb-labs
ln -s /path/to/traefik-plugin-orca plugins-local/src/github.com/dsb-labs/traefik-plugin-orca
```

Then declare it under `localPlugins` in place of `plugins`, with the same
`moduleName` and no `version`.

| Option | Default | Description |
|---|---|---|
| `endpoint` | `http://127.0.0.1:7373` | The base URL of the orca API. |
| `pollInterval` | `5s` | How often to read the services, as a Go duration. |

## Labelling a service

The plugin only reads services that carry the `traefik.enable=true` label.
Router configuration comes from further labels on the same service:

```yaml
version: v1
name: home-assistant
labels:
  traefik.enable: "true"
  traefik.http.routers.home-assistant.rule: Host(`ha.lab.dsb.dev`)
  traefik.http.routers.home-assistant.entrypoints: https
  traefik.http.routers.home-assistant.tls.certresolver: cloudflare
target:
  labels:
    app: home-assistant
  port: 8123
```

The traefik service is always generated, named after the orca service. A router
that names no `service` refers to it. A router's name must not contain dots,
because a dot in a label key separates path segments.

The plugin decodes the whole label tree the
[docker provider](https://doc.traefik.io/traefik/reference/routing-configuration/other-providers/docker/)
defines, against the same configuration types traefik publishes for plugins
([genconf](https://github.com/traefik/genconf)). Routers, middlewares, TCP and
UDP routing, sticky sessions, health checks and servers transports all work
from labels. A label that does not decode is logged and skipped, so a typo
surfaces in traefik's log without taking the backends out of rotation.

A bare `true` enables an element without setting its options, the way the
docker provider treats `traefik.http.routers.<name>.tls=true` or
`traefik.http.middlewares.<name>.stripprefix=true`.

Some limits apply:

- Orca refuses label keys over 63 characters, which a deep path such as
  `...loadbalancer.sticky.cookie.httponly` exceeds. Keep names short.
- The `traefik.http.services.<name>.loadbalancer.server.` labels are refused,
  except `server.scheme` on the service's own name, which sets the scheme
  backend URLs are written with. The other server fields describe what orca
  already provides.
- The configuration types lag traefik itself a little. The affinity cookie
  has no `maxage`, `path` or `domain`, the health check has no `status`, and
  the compress middleware has no `encodings`, which traefik v3 requires, so
  enable compression through another provider.
- List values are comma-separated. A label key cannot hold a slice index, so
  options that need one, such as a middleware chain's list of certificates,
  cannot be written as labels.

## Protocols

The orca target's protocol picks the traefik section:

- A `tcp` target fills the HTTP section, with backends written as
  `http://host:port` URLs. When the labels declare `traefik.tcp.routers.`
  entries the backends fill the TCP section instead, addressed as `host:port`.
- A `udp` target fills the UDP section.

An opted-in service with no router labels still produces the generated traefik
service. A router held by another provider can reference it as
`<name>@plugin-orca`.

## Behaviour to know

- Backends are the instances orca reports fit to serve, so an instance that
  fails its health check leaves the rotation on the next poll.
- A service with no backends produces a traefik service with no servers.
  Its routers stay defined and traefik answers 503 until backends arrive.
- A failed poll keeps the last configuration, so a briefly unreachable orca
  server does not empty traefik's routing table.
- Sticky sessions pin a client to a backend URL. Orca reallocates host ports
  when it replaces an instance, so the cookie stops matching and traefik
  re-balances that client on its next request.
- The `healthcheck` labels run traefik's own probe on top of orca's health
  check. Orca's check gates which backends are reported at each poll, while
  traefik's reacts between polls and sees failures on traefik's own network
  path.
