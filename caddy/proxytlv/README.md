# proxytlv — PROXY v2 TLVs for caddy-l4

A [caddy-l4](https://github.com/mholt/caddy-l4) handler,
`layer4.handlers.proxy_tlv`, for a Caddy that terminates TLS in front of a
TCP service and needs to tell that service what it saw.

It writes one **PROXY protocol v2** header ahead of the client's stream:

| Header part | Value |
|---|---|
| source / destination | the real client, and the address it dialled (taken from an earlier `proxy_protocol` handler when there is one, else the socket) |
| `PP2_TYPE_AUTHORITY` (`0x02`) | the SNI hostname |
| `PP2_TYPE_ALPN` (`0x01`) | the negotiated application protocol, if any |
| `PP2_TYPE_SSL` (`0x20`) | client-leg TLS, with `PP2_SUBTYPE_SSL_VERSION` (`"TLS 1.3"`) |

That header is the whole contract. The receiver needs to know nothing
about Caddy — HAProxy's `send-proxy-v2-ssl` with
`proxy-v2-options authority` produces the same thing — and the thanks,
computer chassis reads it on a [`;proxy=` tcp listener](../../docs/advanced/protocols/tcp.md#behind-a-tls-terminating-edge),
where the SNI becomes the hostname a connection is routed by.

**Why it exists:** caddy-l4's `proxy` handler can send a PROXY header, but
without TLVs, so the hostname never reaches the upstream. When `proxy`
learns to emit them, delete this module.

## Use

```sh
xcaddy build \
  --with github.com/mholt/caddy-l4@b02a6fd06dafbaa1f950d099eb73e4ac77d70b06 \
  --with github.com/loremlabs/thanks-computer/caddy/proxytlv@<commit-or-tag>
```

```caddyfile
{
	layer4 {
		:6697 {
			route {
				# optional: the hop in front of Caddy speaks PROXY too
				proxy_protocol {
					allow 172.16.0.0/12
					timeout 2s
				}
				tls
				proxy_tlv
				proxy {
					upstream chassis.internal:16697
				}
			}
		}
	}
}
```

Order matters: after `tls` (there is nothing to report before the
handshake), before `proxy`. Do **not** also set `proxy_protocol` on the
`proxy` handler — the upstream would get two headers. `proxy_tlv` takes no
options.

The upstream must only accept this header from the edge's addresses; on
the chassis that is the listener's `;proxy=CIDR|CIDR` list.

## Notes

- A nested Go module on purpose: `xcaddy --with` resolves only this
  directory's `go.mod` (Caddy, caddy-l4, go-proxyproto), never the chassis's
  dependency graph. It imports nothing from `chassis/`. Tags take the
  nested-module form, `caddy/proxytlv/v0.1.0`.
- Pinned to the caddy-l4 commit and go-proxyproto version the edge already
  builds (the last caddy-l4 on the Caddy 2.10 line), so adding it changes
  no other module's version.
- No client-certificate TLVs: the edge does not ask for client certs.
  `PP2_TYPE_SSL`'s verify field is therefore always non-zero.

## Test

```sh
go test -race ./...
```

The root `make test` runs this too. For a live check, point `upstream` at
`nc -l 16697 | xxd` and connect with
`openssl s_client -connect localhost:6697 -servername irc.example.test`:
the dump starts with the v2 signature (`0d0a0d0a000d0a515549540a`) and
carries the `0x20`, `0x02` and `0x01` TLVs.
