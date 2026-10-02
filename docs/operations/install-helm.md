# Install with Helm

The Helm chart deploys suxen with an optional bundled PostgreSQL cluster, ServiceMonitor,
disruption budget, and autoscaling. The [Helm chart guide](../../charts/suxen/README.md)
is the authoritative reference for every value; this page is the entry point.

## Install a published chart

Install a published chart with an explicit immutable version:

```sh
helm install suxen \
  oci://ghcr.io/suxen-project/suxen-chart \
  --version 1.0.0-rc.2
```

The chart uses the standard image matching its app version by default. Disable its UI
routes at runtime without changing the image:

```yaml
config:
  uiEnabled: false
```

Runtime disabling does not remove the embedded assets. Published images and the chart's
default image are always the standard UI-capable build; the workflow never pushes a `noui`
image.

## Single-node vs. multi-replica

A single-replica release can use SQLite on a persistent volume. For a stateless,
fault-tolerant deployment on PostgreSQL and S3 or GCS object storage, see
[high availability](high-availability.md), which includes a complete three-replica values
file and the credential Secrets the chart expects.

The chart refuses multiple replicas or autoscaling unless cluster mode, shared PostgreSQL,
a shared blob store (an S3-compatible store, GCS, or a `ReadWriteMany` PVC), and a bootstrap
credential Secret are configured together.

## Extra OCI registry hosts and ports

Put OCI `endpoints.hosts` and `endpoints.ports` on the repository in
`provisioning.document`. The chart derives extra Service/container ports and extra
Ingresses from that document so Helm values are not a second copy.

```yaml
provisioning:
  enabled: true
  document: |
    apiVersion: suxen.io/v1
    resources:
      - kind: repository
        name: docker
        spec:
          format: oci
          type: hosted
          endpoints:
            hosts:
              - registry.example.com
            ports:
              - 5000

ingress:
  enabled: true
  hosts:
    - host: suxen.example.com
      paths:
        - path: /
          pathType: Prefix
  ociHosts:
    registry.example.com:
      tls:
        - secretName: suxen-registry-tls
```

Extra Ingresses use the primary Service port (`http`), which is the HA Docker model.
`ingress.ociHosts` is only TLS, annotations, and an optional `servicePort`; its keys must
match `endpoints.hosts`. Hosts already listed under `ingress.hosts` do not get a second
Ingress.

`service.extraPorts` and `ingress.extraHosts` remain for `provisioning.existingConfigMap`
or repositories created outside the inline document. Combining them with endpoints in
`provisioning.document` fails the render. Extra container ports are registry-only
(`/v2` and probes). Every pod binds a newly created extra port only after a restart. See
[Repositories](../guides/repositories.md#oci-registry-roots).
