# Contributing to tt-dra-driver

Thank you for your interest in contributing. This document explains how to
report problems, propose changes, and get a pull request merged.

## Code of Conduct

This project follows the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md).
By participating you agree to uphold it. Report unacceptable behavior to
ospo@tenstorrent.com.

## Reporting bugs and requesting features

- Report bugs and request features through
  [GitHub Issues](https://github.com/tenstorrent/tt-dra-driver/issues).
- Before opening an issue, search existing issues to avoid duplicates.
- For bugs, include the driver version, Kubernetes version, the Tenstorrent
  Fabric Manager version, the `ResourceClaim` and Pod manifests involved, and
  relevant kubelet plugin logs.
- Do **not** report security vulnerabilities through public issues. Follow the
  process in [SECURITY.md](SECURITY.md) instead.

## Submitting changes

Bug fixes and new functionality are submitted as pull requests against the
`main` branch.

1. Fork the repository and create a branch from `main`.
2. Make your change, including tests where the change affects behavior.
3. Run the checks listed below and make sure they pass.
4. Open a pull request. Describe what the change does and why, and link any
   related issues.

Pull requests are reviewed on a weekly cadence. A maintainer may ask for
changes before merging. Pull requests are merged with a squash merge, so keep
the pull request title and description accurate; they become the commit
message on `main`.

### Developer workflow

```bash
make binaries     # build the kubelet plugin binary
make check        # fmt, vet, and lint (golangci-lint, see .golangci.yaml)
make test         # run unit tests
make coverage     # run tests with coverage
make gen-proto    # regenerate Go code from the Fabric Manager protobuf definitions
```

Each target can also run inside the development container image with
`make docker-<target>`.

If you change anything under `fm-proto/`, run `make gen-proto` and commit the
regenerated `*.pb.go` files together with your change.

### Coding standards

- Format Go code with `gofmt`; `make check` fails on unformatted code.
- Fix all `golangci-lint` findings before opening a pull request.
- Add or update documentation under `docs/` and the README for any
  user-visible change to device attributes, the `DeviceClass`, or Helm values.

### License headers

Every source file must carry an SPDX header. For new files written for this
project, use:

```go
// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.
```

Use the comment syntax appropriate to the file type (`#` for shell, Makefile,
Dockerfile, and YAML). Do not modify the headers of third-party files.

### Commit messages

Use a short imperative summary line (72 characters or fewer), optionally
followed by a blank line and a longer explanation of the motivation for the
change.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
