# Building octo-server

## Dependencies

This project depends on several sibling repositories in the OCTO ecosystem:

- [octo-lib](https://github.com/Mininglamp-OSS/octo-lib) — core shared library
- [octo-adapters](https://github.com/Mininglamp-OSS/octo-adapters) — AI agent adapters

While these repositories are private during the pre-release phase,
`go build ./...` may fail with "missing go.sum entry" errors.

## Local build (private preview)

1. Clone the sibling repositories alongside this repo:
   ```
   git clone git@github.com:Mininglamp-OSS/octo-lib.git
   git clone git@github.com:Mininglamp-OSS/octo-server.git
   ```

2. Add a `replace` directive to your local `go.mod`:
   ```
   replace github.com/Mininglamp-OSS/octo-lib => ../octo-lib
   ```

3. Run `go mod tidy && go build ./...`

## Public build

Once all OCTO repositories are public, the standard Go toolchain
will resolve imports from `proxy.golang.org` automatically.

## Docker

For an end-to-end OCTO stack (this server plus admin / web / matter /
smart-summary / WuKongIM / MySQL / Redis / MinIO / nginx), see the
official OOTB deployment at
[`Mininglamp-OSS/octo-deployment`](https://github.com/Mininglamp-OSS/octo-deployment).
The older `docker/octo/` and `docker/tsdd/` compose stacks that used
to ship in this repository have been retired in favour of that single
source of truth.

To build only the `octo-server` container image from this repository:

```bash
make build          # docker build -t octo-server .
```

Tag / push targets (publishing to the team registry) are defined in
the `Makefile` (`make push`, `make deploy`, `make deploy-v2`).
