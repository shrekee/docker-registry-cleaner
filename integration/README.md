# Disposable Distribution integration tests

These opt-in tests start a new server bound to `127.0.0.1`, with temporary filesystem storage and no credentials. They never use a user registry. The test harness uses regclient to seed OCI images and a tagged index/child. A loopback proxy injects specific failures; this does **not** emulate a production registry or prove compatibility with one.

The test suite expects official Distribution v3.0.0 and v2.8.3 executables. For example, from clean public sources using Go 1.27.3, in an isolated scratch directory:

```sh
mkdir -p /tmp/drc-test/gopath/src/github.com/docker
# Verify tags/commits against the official repository before building.
git clone --depth 1 --branch v3.0.0 https://github.com/distribution/distribution.git /tmp/drc-test/v3
(cd /tmp/drc-test/v3 && go build -o /tmp/drc-test/distribution-v3 ./cmd/registry)
git clone --depth 1 --branch v2.8.3 https://github.com/distribution/distribution.git /tmp/drc-test/v2
ln -s /tmp/drc-test/v2 /tmp/drc-test/gopath/src/github.com/docker/distribution
GO111MODULE=off GOPATH=/tmp/drc-test/gopath go build -o /tmp/drc-test/distribution-v2 github.com/docker/distribution/cmd/registry
DISTRIBUTION_V3_BIN=/tmp/drc-test/distribution-v3 \
DISTRIBUTION_V2_BIN=/tmp/drc-test/distribution-v2 \
  ./integration/run-distribution.sh
```

The script builds the cleaner once, prints its SHA-256, and tests that exact executable. The v3 and v2 upstream commits tested are `9ed95e7365224025ee89365e12cf128e1f1bf965` and `4772604ae973031ab32dd9805a4bccf61d94909f` respectively. Unset `DISTRIBUTION_BIN` or `CLEANER_BIN` makes ordinary `go test ./...` **skip** this opt-in integration test; a skip is not an integration pass.

Coverage: dry-run/no writes; explicit preflight requirements; v3 native deletion; v2 default rejection and explicit compatibility mode; 405 fallback; failed fallback PUT with no subsequent write; applied DELETE with lost response and no blind retry; stale preflight; incomplete pagination rejection; no-op 202 postflight rejection; protected aliases, tagged index/child and original manifest digest retained after tag removal. This does not test TLS, authentication, network partitions beyond the injected faults, GC, production races, registry vendors, or complete graph visibility.
