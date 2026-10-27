---
title: Wake a suspended llama-swap server with wol-proxy
summary: wol-proxy sits in front of llama-swap, sends a Wake-on-LAN packet when a request arrives and the server is asleep, and holds the request until the server is back.
category: guides
tags: [wol-proxy, wake-on-lan, wol, suspend, power, energy, tools]
updated: 2026-10-27
---

# Wake a suspended llama-swap server with wol-proxy

GPU machines can draw hundreds of watts while idle. `wol-proxy` lets you
suspend the llama-swap machine and still use it: clients talk to wol-proxy,
which runs on an always-on machine on the same LAN. When a request arrives and
llama-swap is not reachable, wol-proxy sends a Wake-on-LAN packet and holds the
request until llama-swap is up, then forwards it.

Build it from the repository root:

```bash
go build -o wol-proxy ./cmd/wol-proxy
```

## Run it

```bash
./wol-proxy -mac BA:DC:0F:FE:E0:00 -upstream http://192.168.1.13:8080
```

| flag | default | meaning |
| --- | --- | --- |
| `-mac` | required | MAC address of the llama-swap machine |
| `-upstream` | required | llama-swap URL |
| `-listen` | `127.0.0.1:8080` | where wol-proxy listens |
| `-timeout` | `60` | seconds a request waits for the server before failing with `408` |
| `-api-key` | none | key for llama-swap's `apiKeys`; also read from `LLAMA_SWAP_API_KEY` |
| `-require-confirm` | off | ask before waking (below) |
| `-log` | `info` | `debug`, `info`, `warn` or `error` |

The default listens on loopback only. `-listen :8080` binds every interface,
which publishes the whole upstream API to the network, `/unload` included;
wol-proxy adds no authentication of its own. Do that only on a trusted LAN.

## How it knows the server is up

wol-proxy keeps a server-sent-events connection open to llama-swap's
`/api/events`. Connected means ready; any failure or disconnect means not ready.
It also checks `/wol-health` every 10 seconds, so a machine that suspended without
closing the connection is noticed. `GET /status` on wol-proxy shows the current
state.

## Browsers and confirmation

Requests for `/` and `/ui/` get a loading page with a progress bar instead of
being held. With `-require-confirm`, nothing wakes the server automatically:
browsers see a "Start server" button, and API calls fail at once with
`503 Service Unavailable`. Use it if background clients (health monitors, editor
plugins) would otherwise keep waking the machine.

The button posts to `/wol-wake`, and wol-proxy accepts that post only from its
own pages. A browser request passes when `Sec-Fetch-Site` is `same-origin` or
`none`; without that header, `Origin` or `Referer` must match the host
wol-proxy was reached on. A page on another site that posts to the endpoint
gets `403 Forbidden` and no packet is sent, so a cross-site form cannot wake the
machine. Non-browser clients send neither header and are accepted.

## What goes wrong

- **Server never wakes.** Wake-on-LAN must be enabled in the BIOS and on the
  network card. The packet is a UDP broadcast, so wol-proxy must be on the same
  network segment; it will not cross a router.
- **Always "not ready", requests time out (`408`).** If llama-swap sets
  `apiKeys`, pass `-api-key`. Without it wol-proxy cannot read `/api/events`.
- **Request fails after `-timeout`.** The machine took longer than the timeout to
  resume and for llama-swap to start. Raise `-timeout`, and make sure llama-swap
  starts at boot.
- **Clients still see errors on the first request.** Clients with their own short
  timeouts give up before the server is awake. Raise the client timeout or use
  `-require-confirm` and wake the server from the browser first.
