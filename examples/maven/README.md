# Maven / Gradle artifacts

Three repositories that work together: `maven-proxy` caches Maven Central,
`maven-hosted` accepts internal artifact deploys, and `maven` (a group over
both) is the single repository URL that serves internal artifacts and cached
public ones.

## Server side

```sh
examples/maven/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml). Prints the URL and admin token; Ctrl-C to stop.

`maven-hosted` sets `formatConfig.versionPolicy: release`, so it accepts only
release versions and refuses `-SNAPSHOT` deploys. Use `snapshot` for a
snapshot-only repository, or `mixed` (the default) to accept both.

Groups merge snapshot metadata per classifier and extension. The newest update
wins; equal update timestamps are resolved by the timestamped version and numeric
build number. A classifier omitted from a newer member remains available from
the member that supplies it.

Cleanup policies on `maven-hosted` can select `maven.groupId` and
`maven.artifactId` with `keepLast`. For example, `keepLast: 1` and criterion
`{"path":"maven.artifactId","op":"=","value":"widget"}` retain the
newest stored `widget` version. A version's POM, JAR, classifiers, checksums,
and signatures move together. If any file in its version directory does not
match all criteria, cleanup preserves that directory. Stored SNAPSHOT version-level
metadata also participates in this check. Timestamped SNAPSHOT builds within
one base-version directory count as one version.

For metadata `latest`/`release` and `keepLast`, suxen compares versions using
Maven 3.9.16's separator and qualifier rules. Unicode decimal digits and
case-insensitive qualifiers follow the Unicode tables in the Go build;
Maven's own result for newly added Unicode characters can vary with its JVM.

## Client side

### Resolve from a private repository

Point a repository at the group URL. Put a dedicated local username and account password
in `settings.xml` under a `<server>` with the same `id` (`suxen` below):

```xml
<!-- ~/.m2/settings.xml -->
<settings>
  <servers>
    <server>
      <id>suxen</id>
      <username>ci</username>
      <password>${env.MAVEN_CI_PASSWORD}</password>
    </server>
  </servers>
</settings>
```

```xml
<!-- pom.xml -->
<repositories>
  <repository>
    <id>suxen</id>
    <url>${env.SUXEN_URL}/repository/maven</url>
  </repository>
</repositories>
```

Or resolve one artifact directly:

```sh
mvn org.apache.maven.plugins:maven-dependency-plugin:3.8.1:get \
  -Dartifact=org.apache.commons:commons-lang3:3.14.0 \
  -DremoteRepositories="suxen::default::$SUXEN_URL/repository/maven"
```

Gradle:

```groovy
repositories {
    maven {
        url = uri("${System.getenv('SUXEN_URL')}/repository/maven")
        credentials {
            username = 'ci'
            password = System.getenv('MAVEN_CI_PASSWORD')
        }
    }
}
```

### Deploy to hosted

Deploying needs credentials with `repository:maven-hosted:write`. Attach the
`maven-publisher` role from `repo.yaml` to a scoped user by applying a second
document (its password comes from the environment, so nothing is committed):

```yaml
apiVersion: suxen.io/v1
resources:
  - kind: user
    name: ci
    spec:
      admin: false
      roles: [maven-publisher]
      secretRef:
        env: MAVEN_CI_PASSWORD
```

```sh
export MAVEN_CI_PASSWORD=...
suxenctl apply -f ci-user.yaml
```

Put the credentials in `~/.m2/settings.xml` under a `<server>` whose `id`
matches the repository id, then publish with Gradle's `maven-publish` plugin
pointed at `$SUXEN_URL/repository/maven-hosted`, or `mvn deploy`. The group and
proxy repositories refuse writes (`405`); only `maven-hosted` accepts them.

> `mvn deploy:deploy-file` does not currently write `maven-metadata.xml` on the
> hosted repository, so prefer Gradle's `maven-publish` (or a project `mvn
> deploy`) for publishing; exact-version resolves work either way.

## Test

[`test.bats`](test.bats) covers both paths in a pinned `maven` image:

1. deploying a jar + pom to **maven-hosted** and resolving it back with Maven's
   Basic-auth `<server>` credentials. The artifact comes only from suxen;
   Maven may still download the `dependency:get` plugin from Central unless
   it is already cached. Deploy is an HTTP PUT to the coordinate path, as
   `mvn deploy` and `gradle publish` do under the hood;
2. resolving `commons-lang3` through the **group** — this fetches from Maven
   Central via the proxy member, so it needs network egress and **skips** when
   Central cannot be reached.

Run it with `bats examples/maven/test.bats` (needs `bats` and Docker).
