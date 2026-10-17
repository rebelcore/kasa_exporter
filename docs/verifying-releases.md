# Verifying releases

Every release after 1.0.1 can be checked three ways: its checksums and container
image carry Sigstore signatures, its git tag is GPG-signed, and its Linux
binaries can be rebuilt bit-for-bit from the tag. The commands below use
`v1.2.3` as the release; substitute the one you downloaded.

## Release archives

Each GitHub release includes `sha256sums.txt` and a Sigstore bundle for it,
`sha256sums.txt.sigstore.json`. The release workflow uses Sigstore's keyless
signing, so there is no long-lived private key to steal. With
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
installed, download the archive you want, `sha256sums.txt` and the bundle into
one directory, then run:

```bash
cosign verify-blob \
  --bundle sha256sums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/rebelcore/kasa_exporter/\.github/workflows/publish\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  sha256sums.txt
sha256sum --check --ignore-missing sha256sums.txt
```

The first command shows that `sha256sums.txt` was signed by this repository's
release workflow for a version tag. The second checks your download against it.

## Container images

```bash
cosign verify rebelcore/kasa-exporter:v1.2.3 \
  --certificate-identity-regexp '^https://github\.com/rebelcore/kasa_exporter/\.github/workflows/publish\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Git tags

Release tags are signed by RebelBot, and GitHub shows them as Verified. To check
one locally, import RebelBot's public key and verify the tag:

```bash
curl -fsSL https://github.com/rebel-bot.gpg | gpg --import
git tag -v v1.2.3
```

## Rebuilding a release

Release binaries are built with [promu](https://github.com/prometheus/promu) in
the `quay.io/prometheus/golang-builder` image, with `SOURCE_DATE_EPOCH` set to
the tagged commit's time. promu then stamps that date, and `reproducible` in
place of the build user and host, so the same tag and Go version always produce
the same binary. To rebuild the Linux amd64 binary you need Go, Docker and
promu 0.20.0 or later, which `make promu` installs:

```bash
git clone https://github.com/rebelcore/kasa_exporter.git
cd kasa_exporter
git checkout v1.2.3
make promu
export SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)"
"$(go env GOPATH)/bin/promu" crossbuild --env "SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}" -p linux/amd64
sha256sum .build/linux-amd64/kasa_exporter
```

Then compare it with the binary from the release archive:

```bash
tar -xzOf kasa_exporter-1.2.3.linux-amd64.tar.gz \
  kasa_exporter-1.2.3.linux-amd64/kasa_exporter | sha256sum
```

The builder image moves to each new Go patch release, and a different Go version
produces a different binary. Rebuild while the image still ships the Go version
the release reports in `kasa_exporter --version`.

## 1.0.0 and 1.0.1

These releases predate Sigstore signing and reproducible builds. Their
`sha256sums.txt` is signed with RebelBot's GPG key instead, and covers the
tarballs but not the Windows zips:

```bash
curl -fsSL https://github.com/rebel-bot.gpg | gpg --import
gpg --verify sha256sums.txt.asc sha256sums.txt
sha256sum --check --ignore-missing sha256sums.txt
```

1.0.0 also has a detached `.asc` signature beside each tarball.
