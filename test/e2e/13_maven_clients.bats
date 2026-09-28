#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
  e2e_gradle_user_home=""
}

teardown() {
  # Wait for the daemon before the runner removes its workspace. Only the
  # Gradle scenario starts one.
  if [[ -n "${e2e_gradle_user_home:-}" ]]; then
    gradle --stop \
      --gradle-user-home "${e2e_gradle_user_home}" >/dev/null
  fi
}

write_maven_settings() {
  local output="$1"
  cat >"${output}" <<EOF
<settings xmlns="http://maven.apache.org/SETTINGS/1.2.0">
  <mirrors>
    <!-- Maven blocks external HTTP repositories by default. The CI client
         reaches this disposable test registry through Docker's bridge gateway,
         so exempt only its repository ID and retain the blocker everywhere
         else. Developer loopback runs do not need the exception but use the
         same settings to keep both environments identical. -->
    <mirror>
      <id>maven-default-http-blocker</id>
      <mirrorOf>external:http:*,!suxen-e2e</mirrorOf>
      <name>Block external HTTP repositories except the E2E registry</name>
      <url>http://0.0.0.0/</url>
      <blocked>true</blocked>
    </mirror>
  </mirrors>
  <servers>
    <server>
      <id>suxen-e2e</id>
      <username>admin</username>
      <password>admin-password</password>
    </server>
  </servers>
</settings>
EOF
}

write_artifact_pom() {
  local output="$1"
  local group="$2"
  local artifact="$3"
  local version="$4"
  cat >"${output}" <<EOF
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>${group}</groupId>
  <artifactId>${artifact}</artifactId>
  <version>${version}</version>
</project>
EOF
}

# Path of the jar Maven stores for a coordinate inside the shared local
# repository, used to assert the exact bytes a resolution produced.
cached_jar_path() {
  local group="$1"
  local artifact="$2"
  local version="$3"
  local group_path="${group//./\/}"
  printf '%s' \
    "${MAVEN_CACHE}/${group_path}/${artifact}/${version}/${artifact}-${version}.jar"
}

# Path of the jar seeded on the raw upstream for a coordinate, the expected
# bytes for a proxy resolution.
upstream_jar_path() {
  local artifact="$1"
  local version="$2"
  printf '%s' "${E2E_CASE_DIRECTORY}/upstream-${artifact}/${artifact}-${version}.jar"
}

write_hosted_artifact() {
  local group="$1"
  local artifact="$2"
  local version="$3"
  local directory="${E2E_CASE_DIRECTORY}/hosted-${artifact}"
  mkdir -p "${directory}"
  printf 'maven hosted artifact %s\n' "${artifact}" \
    >"${directory}/${artifact}-${version}.jar"
  write_artifact_pom \
    "${directory}/${artifact}-${version}.pom" \
    "${group}" \
    "${artifact}" \
    "${version}"
}

hosted_jar_path() {
  local artifact="$1"
  local version="$2"
  printf '%s' "${E2E_CASE_DIRECTORY}/hosted-${artifact}/${artifact}-${version}.jar"
}

seed_maven_upstream() {
  local group="$1"
  local artifact="$2"
  local version="$3"
  local group_path="${group//./\/}"
  local base_path="${group_path}/${artifact}/${version}"
  local artifact_prefix="${artifact}-${version}"
  local seed_directory="${E2E_CASE_DIRECTORY}/upstream-${artifact}"
  mkdir -p "${seed_directory}"
  printf 'upstream artifact %s\n' "${artifact}" >"${seed_directory}/${artifact_prefix}.jar"
  write_artifact_pom \
    "${seed_directory}/${artifact_prefix}.pom" \
    "${group}" \
    "${artifact}" \
    "${version}"

  local extension
  for extension in jar pom; do
    curl \
      --fail-with-body \
      --silent \
      --show-error \
      --user upstream:upstream-password \
      --upload-file "${seed_directory}/${artifact_prefix}.${extension}" \
      "${SUXEN_E2E_RAW_UPSTREAM}/files/maven/${base_path}/${artifact_prefix}.${extension}"
  done
}

# All Maven invocations share one local repository so the deploy and dependency
# plugins download once. Every resolution below uses a distinct coordinate, so
# the shared repository can never satisfy a later route from an earlier route's
# download, and each resolution asserts the resolved bytes.
maven_resolve() {
  local repository="$1"
  local coordinates="$2"
  mvn \
    --batch-mode \
    --no-transfer-progress \
    --settings "${E2E_CASE_DIRECTORY}/settings.xml" \
    -Dmaven.repo.local="${MAVEN_CACHE}" \
    org.apache.maven.plugins:maven-dependency-plugin:3.8.1:get \
    -Dartifact="${coordinates}" \
    -DremoteRepositories="suxen-e2e::default::${SUXEN_E2E_URL}/repository/${repository}" \
    -Dtransitive=false
}

maven_deploy() {
  local repository="$1"
  local file="$2"
  local pom="$3"
  mvn \
    --batch-mode \
    --no-transfer-progress \
    --settings "${E2E_CASE_DIRECTORY}/settings.xml" \
    -Dmaven.repo.local="${MAVEN_CACHE}" \
    org.apache.maven.plugins:maven-deploy-plugin:3.1.4:deploy-file \
    -DrepositoryId=suxen-e2e \
    -Durl="${SUXEN_E2E_URL}/repository/${repository}" \
    -Dfile="${file}" \
    -DpomFile="${pom}"
}

write_gradle_resolver() {
  local directory="$1"
  local repository="$2"
  local coordinates="$3"
  mkdir -p "${directory}"
  cat >"${directory}/settings.gradle" <<'EOF'
rootProject.name = 'suxen-e2e-resolver'
EOF
  cat >"${directory}/build.gradle" <<EOF
repositories {
    maven {
        url = uri("${SUXEN_E2E_URL}/repository/${repository}")
        allowInsecureProtocol = true
        credentials {
            username = "admin"
            password = "admin-password"
        }
    }
}

configurations {
    artifact
}

dependencies {
    artifact "${coordinates}"
}

tasks.register("resolveArtifact") {
    doLast {
        def resolved = configurations.artifact.singleFile
        copy {
            from resolved
            into layout.buildDirectory.dir("resolved")
        }
    }
}
EOF
}

# All Gradle invocations share one user home and daemon so the daemon warms and
# modules download once. Distinct coordinates keep Gradle's module cache from
# satisfying a later route without contacting suxen, and each resolution copies
# the resolved file out so its bytes can be asserted.
gradle_run() {
  local directory="$1"
  shift
  gradle \
    --daemon \
    --console plain \
    --gradle-user-home "${e2e_gradle_user_home}" \
    --project-dir "${directory}" \
    "$@"
}

gradle_resolve() {
  local repository="$1"
  local coordinates="$2"
  local directory="$3"
  write_gradle_resolver "${directory}" "${repository}" "${coordinates}"
  gradle_run "${directory}" resolveArtifact
}

@test "Maven deploys only to hosted and resolves hosted, proxy, and group repositories" {
  require_full_interop
  require_command java
  require_command mvn

  MAVEN_CACHE="${E2E_CASE_DIRECTORY}/maven-cache"
  local hosted_group="dev.suxen.e2e"
  local proxy_group="dev.suxen.upstream"
  write_maven_settings "${E2E_CASE_DIRECTORY}/settings.xml"

  write_hosted_artifact "${hosted_group}" maven-hosted-direct 1.0.0
  write_hosted_artifact "${hosted_group}" maven-group-hosted 1.1.0

  # Deploy is accepted only by the hosted repository.
  run maven_deploy maven-hosted \
    "$(hosted_jar_path maven-hosted-direct 1.0.0)" \
    "${E2E_CASE_DIRECTORY}/hosted-maven-hosted-direct/maven-hosted-direct-1.0.0.pom"
  assert_success

  run maven_deploy maven-proxy \
    "$(hosted_jar_path maven-hosted-direct 1.0.0)" \
    "${E2E_CASE_DIRECTORY}/hosted-maven-hosted-direct/maven-hosted-direct-1.0.0.pom"
  assert_failure
  assert_contains "${output}" "405"

  run maven_deploy maven-group \
    "$(hosted_jar_path maven-hosted-direct 1.0.0)" \
    "${E2E_CASE_DIRECTORY}/hosted-maven-hosted-direct/maven-hosted-direct-1.0.0.pom"
  assert_failure
  assert_contains "${output}" "405"

  # The group route resolves this hosted-only coordinate through its hosted
  # member, so it must reach the hosted repository.
  run maven_deploy maven-hosted \
    "$(hosted_jar_path maven-group-hosted 1.1.0)" \
    "${E2E_CASE_DIRECTORY}/hosted-maven-group-hosted/maven-group-hosted-1.1.0.pom"
  assert_success

  seed_maven_upstream "${proxy_group}" maven-proxy-direct 2.0.0
  seed_maven_upstream "${proxy_group}" maven-group-proxy 2.1.0

  # Hosted route.
  run maven_resolve maven-hosted "${hosted_group}:maven-hosted-direct:1.0.0"
  assert_success
  run cmp \
    "$(cached_jar_path "${hosted_group}" maven-hosted-direct 1.0.0)" \
    "$(hosted_jar_path maven-hosted-direct 1.0.0)"
  assert_success

  # Proxy route.
  run maven_resolve maven-proxy "${proxy_group}:maven-proxy-direct:2.0.0"
  assert_success
  run cmp \
    "$(cached_jar_path "${proxy_group}" maven-proxy-direct 2.0.0)" \
    "$(upstream_jar_path maven-proxy-direct 2.0.0)"
  assert_success

  # Group route over its hosted member.
  run maven_resolve maven-group "${hosted_group}:maven-group-hosted:1.1.0"
  assert_success
  run cmp \
    "$(cached_jar_path "${hosted_group}" maven-group-hosted 1.1.0)" \
    "$(hosted_jar_path maven-group-hosted 1.1.0)"
  assert_success

  # Group route over its proxy member.
  run maven_resolve maven-group "${proxy_group}:maven-group-proxy:2.1.0"
  assert_success
  run cmp \
    "$(cached_jar_path "${proxy_group}" maven-group-proxy 2.1.0)" \
    "$(upstream_jar_path maven-group-proxy 2.1.0)"
  assert_success
}

@test "Gradle publishes only to hosted and resolves hosted, proxy, and group repositories" {
  require_full_interop
  require_command java
  require_command gradle

  # Gradle can still write cache and project files after a task returns.
  # Keep those paths outside Bats' temporary tree so its own cleanup does
  # not race the daemon.
  E2E_CASE_DIRECTORY="${SUXEN_E2E_GRADLE_DIRECTORY}"
  mkdir -p "${E2E_CASE_DIRECTORY}"
  e2e_gradle_user_home="${E2E_CASE_DIRECTORY}/gradle-home"
  local publish_directory="${E2E_CASE_DIRECTORY}/gradle-publisher"
  local hosted_group="dev.suxen.e2e"
  local proxy_group="dev.suxen.upstream"
  mkdir -p "${publish_directory}/src/main/java/dev/suxen/e2e"
  cat >"${publish_directory}/settings.gradle" <<'EOF'
rootProject.name = 'gradle-client'
EOF
  cat >"${publish_directory}/src/main/java/dev/suxen/e2e/Fixture.java" <<'EOF'
package dev.suxen.e2e;

public final class Fixture {
    private Fixture() {
    }
}
EOF
  # The published artifactId, version, and archive name all come from Gradle
  # properties so one project can publish a distinct coordinate per route.
  cat >"${publish_directory}/build.gradle" <<EOF
plugins {
    id 'java-library'
    id 'maven-publish'
}

group = 'dev.suxen.e2e'
version = providers.gradleProperty('publishVersion').getOrElse('1.0.0')

base {
    archivesName = providers.gradleProperty('publishArtifact').getOrElse('gradle-client')
}

def suxenRepository = providers.gradleProperty('suxenRepository')
    .getOrElse('maven-hosted')

publishing {
    publications {
        library(MavenPublication) {
            from components.java
            artifactId = providers.gradleProperty('publishArtifact').getOrElse('gradle-client')
        }
    }
    repositories {
        maven {
            name = 'suxen'
            url = uri("${SUXEN_E2E_URL}/repository/\${suxenRepository}")
            allowInsecureProtocol = true
            credentials {
                username = 'admin'
                password = 'admin-password'
            }
        }
    }
}
EOF

  # Publish the hosted-route coordinate and capture its exact jar bytes.
  run gradle_run "${publish_directory}" \
    -PpublishArtifact=gradle-hosted-direct -PpublishVersion=1.0.0 publish
  assert_success
  cp \
    "${publish_directory}/build/libs/gradle-hosted-direct-1.0.0.jar" \
    "${E2E_CASE_DIRECTORY}/expected-gradle-hosted-direct.jar"

  # Publication is accepted only by the hosted repository.
  run gradle_run "${publish_directory}" \
    -PpublishArtifact=gradle-hosted-direct -PpublishVersion=1.0.0 \
    -PsuxenRepository=maven-proxy publish
  assert_failure
  assert_contains "${output}" "405"

  run gradle_run "${publish_directory}" \
    -PpublishArtifact=gradle-hosted-direct -PpublishVersion=1.0.0 \
    -PsuxenRepository=maven-group publish
  assert_failure
  assert_contains "${output}" "405"

  # Publish a second hosted-only coordinate for the group route to reach through
  # its hosted member, and capture its bytes.
  run gradle_run "${publish_directory}" \
    -PpublishArtifact=gradle-group-hosted -PpublishVersion=1.1.0 publish
  assert_success
  cp \
    "${publish_directory}/build/libs/gradle-group-hosted-1.1.0.jar" \
    "${E2E_CASE_DIRECTORY}/expected-gradle-group-hosted.jar"

  seed_maven_upstream "${proxy_group}" gradle-proxy-direct 2.0.0
  seed_maven_upstream "${proxy_group}" gradle-group-proxy 2.1.0

  # Hosted route.
  run gradle_resolve \
    maven-hosted \
    "${hosted_group}:gradle-hosted-direct:1.0.0" \
    "${E2E_CASE_DIRECTORY}/gradle-resolve-hosted"
  assert_success
  run cmp \
    "${E2E_CASE_DIRECTORY}/gradle-resolve-hosted/build/resolved/gradle-hosted-direct-1.0.0.jar" \
    "${E2E_CASE_DIRECTORY}/expected-gradle-hosted-direct.jar"
  assert_success

  # Proxy route.
  run gradle_resolve \
    maven-proxy \
    "${proxy_group}:gradle-proxy-direct:2.0.0" \
    "${E2E_CASE_DIRECTORY}/gradle-resolve-proxy"
  assert_success
  run cmp \
    "${E2E_CASE_DIRECTORY}/gradle-resolve-proxy/build/resolved/gradle-proxy-direct-2.0.0.jar" \
    "$(upstream_jar_path gradle-proxy-direct 2.0.0)"
  assert_success

  # Group route over its hosted member.
  run gradle_resolve \
    maven-group \
    "${hosted_group}:gradle-group-hosted:1.1.0" \
    "${E2E_CASE_DIRECTORY}/gradle-resolve-group-hosted"
  assert_success
  run cmp \
    "${E2E_CASE_DIRECTORY}/gradle-resolve-group-hosted/build/resolved/gradle-group-hosted-1.1.0.jar" \
    "${E2E_CASE_DIRECTORY}/expected-gradle-group-hosted.jar"
  assert_success

  # Group route over its proxy member.
  run gradle_resolve \
    maven-group \
    "${proxy_group}:gradle-group-proxy:2.1.0" \
    "${E2E_CASE_DIRECTORY}/gradle-resolve-group-proxy"
  assert_success
  run cmp \
    "${E2E_CASE_DIRECTORY}/gradle-resolve-group-proxy/build/resolved/gradle-group-proxy-2.1.0.jar" \
    "$(upstream_jar_path gradle-group-proxy 2.1.0)"
  assert_success
}
