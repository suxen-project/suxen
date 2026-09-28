# Git snapshot proxy (GitHub, AUR, any git host)

suxen's `git` format serves any upstream git repository as **depth-1 snapshots**.
A `git` proxy pointed at a host mirrors its repositories: each clone yields a
shallow, single-snapshot checkout, cached in suxen. This is exactly the shape CI
and build hosts need — `git clone --depth 1`, `actions/checkout`, Docker build
git contexts, AUR recipes — so it covers those without running a full git server.

Why proxy git through suxen: insulate CI and build hosts from upstream rate
limits and outages, pin a clone to a specific commit, and let a locked-down
network reach external repositories through suxen only.

This example runs two proxies:

- `github` over `https://github.com` — for cloning external repositories in CI;
- `aur` over `https://aur.archlinux.org` — for Arch package build recipes.

The same pattern works for any git host: add another `git` proxy with its
upstream.

## Server side

```sh
examples/git/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml). Prints the URL and admin token; Ctrl-C to stop. The example
development profile grants anonymous proxy reads for manual exploration. The automated
example disables that profile and proves a private clone with a local account password.

## Client side

### Generic shallow clone

Clone URLs map straight through — `<suxen>/repository/<proxy>/<path>.git`:

```sh
git clone --depth 1 "$SUXEN_URL/repository/github/octocat/Hello-World.git"
```

For a private repository, store a dedicated local username and account password with a
Git credential helper. API tokens are Bearer credentials and do not work as Basic-auth
passwords.

A `--depth 1` clone (which implies `--single-branch`) is what the snapshot proxy
serves. A plain `git clone` of a repository with several branches or tags is
refused with a message pointing at `--depth 1`, `--single-branch`, or `--branch`;
pushes are refused (git is proxy-only). A commit is cached forever; a branch tip
is re-read from the upstream on the proxy TTL.

### GitHub Actions

Route the git clones your workflow makes for external tooling or dependencies
through suxen, so a job does not hit GitHub directly (fewer rate-limit failures,
a cache you control, and a single egress point):

```yaml
- name: Fetch a pinned external repo through suxen
  run: |
    git clone --depth 1 --branch v1.4.0 \
      "$SUXEN_URL/repository/github/some-org/some-tool.git" tool
env:
  SUXEN_URL: https://suxen.example.com
```

`actions/checkout` already shallow-clones (`fetch-depth: 1` by default), which
the proxy serves; point it at suxen by cloning manually as above, or set the
step's repository URL to the proxy for repositories other than the workflow's
own.

### Arch User Repository (makepkg)

Each AUR package is a single-branch git repository holding a `PKGBUILD` recipe.
Clone it through suxen and build as usual:

```sh
git clone --single-branch "$SUXEN_URL/repository/aur/yay.git"
cd yay
makepkg -si          # builds the package and installs it with pacman
```

suxen mirrors the recipe, not a built package — `makepkg` still does the build,
and `pacman -U` (invoked by `makepkg -i`) installs the result. The AUR's RPC
search endpoint (`aur.archlinux.org/rpc`) is not git and is not proxied, so clone
by exact package name.

> AUR *helpers* like `yay` are not a good fit: their search and dependency
> resolution use the AUR's RPC API (`aur.archlinux.org/rpc`), which is not git
> and is not proxied. suxen serves the git recipes; drive them with `makepkg`.

## What this does and does not do

- **Snapshots, not a mirror.** Clones are shallow with no history, and every
  fetch re-streams the whole snapshot.
- **Recipes, not packages** (for the AUR): suxen serves the `PKGBUILD`; the
  build happens locally.
- **No search API.** Only git is proxied; RPC/search endpoints are not.

## Test

[`test.bats`](test.bats) exercises the two documented flows in a pinned Docker
image:

1. shallow-clone a GitHub repository through the `github` proxy (`alpine/git`) —
   the CI/`actions/checkout` use case;
2. clone an AUR recipe through the `aur` proxy and run `makepkg --printsrcinfo`
   on it (`archlinux:base-devel`) — the Arch toolchain consuming a proxied
   recipe (a full package build is out of scope).

Because the proxies fetch from public upstreams, the tests need network egress
and **skip** when an upstream cannot be reached. Run them with `bats
examples/git/test.bats` (needs `bats` and Docker).
