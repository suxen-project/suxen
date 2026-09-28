# Ephemeral development profile

The example launcher uses an in-memory blob store, a temporary SQLite database, and a
loopback-only listener. It also applies [`repo.yaml`](repo.yaml) after every other
example resource document to grant anonymous artifact reads explicitly:

```sh
examples/suxen-with-mem-blobstore
```

The process prints its one-time development credentials and deletes its temporary data
when it exits. This profile is intended for local examples and client experiments. The
standard binary, Compose installation, runtime image, and Helm chart retain persistent
storage and a private anonymous role.
