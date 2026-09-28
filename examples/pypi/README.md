# PyPI packages

Three repositories that work together: `pypi-proxy` caches the public simple
index (`pypi.org`), `pypi-hosted` accepts internal `twine upload`, and `pypi` (a
group over both) is one `simple/` index serving internal and cached public
distributions.

## Server side

```sh
examples/pypi/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml). Prints the URL and admin token; Ctrl-C to stop.

## Client side

### Publish

Publishing needs write on `pypi-hosted`. Attach the `pypi-publisher` role from
`repo.yaml` to a scoped user by applying a second document (its password comes
from the environment, so nothing is committed):

```yaml
apiVersion: suxen.io/v1
resources:
  - kind: user
    name: ci
    spec:
      admin: false
      roles: [pypi-publisher]
      secretRef:
        env: PYPI_CI_PASSWORD
```

twine authenticates with HTTP Basic auth, which suxen resolves against user
accounts — so publish with a **username and password**, not an API token:

```sh
twine upload --repository-url https://suxen.example.com/repository/pypi-hosted/ \
  -u ci -p "$PYPI_CI_PASSWORD" dist/*
```

### Install

The example launcher grants anonymous reads for manual exploration. A normal fresh
installation is private: configure pip's keyring or `.netrc` with a local username and
account password. Install from the **group**'s `simple/` index to get both internal and
cached public distributions from one URL:

```sh
pip install --index-url https://suxen.example.com/repository/pypi/simple/ <project>
```

Add `--trusted-host suxen.example.com` if the registry is served over plain
HTTP. Public projects requested through the group (or `pypi-proxy` directly) are
fetched from `pypi.org` and cached.

Hosted, proxy, and group indexes negotiate HTML or PEP 691 JSON using the client's
`Accept` preferences. Proxy indexes may link to a separate file host, including
relative links resolved through an HTML `<base href>`; downloads remain routed
through suxen. The JSON response includes a `hashes` object for each file, even
when the upstream supplied no digest. HTML carries one selected hash fragment;
JSON retains every advertised hash.
Fileless JSON `versions` entries and project status survive proxy/group merging.
GPG `.asc` companions advertised by a file link are cached with that file's exact
query, without signature verification. Provenance links remain external URLs.
JSON pages without a known size for every file advertise Simple API 1.0, while
hosted pages include sizes and advertise 1.1.
If any group member marks a project quarantined, the merged project index
contains no files and group downloads for that project are unavailable.
Root and project index responses are bounded to 128 MiB; a root source can
be 128 MiB, an individual project source 8 MiB, and combined group index
sources 256 MiB.

## Test

[`test.bats`](test.bats) covers both halves in a pinned `python` image:

1. publish a wheel with `twine upload` and install it with authenticated `pip` from the
   `simple/` index — the publish/install round-trip is hermetic (hosted, no
   upstream); only installing twine into the image needs network, so it
   **skips** when that download is unavailable;
2. publish distinct wheels to the hosted member and a local source behind a
   proxy, then download both through one group `simple/` index — hermetic after
   installing Twine;
3. `pip download` a public distribution through the **proxy** — needs network
   egress and **skips** when PyPI is unreachable.

Run it with `bats examples/pypi/test.bats` (needs `bats` and Docker).
