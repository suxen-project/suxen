#!/usr/bin/env bats
# Boots the example, then resolves a real artifact through the maven group.
# The group's proxy member fetches from Maven Central, so this test needs
# network egress; it skips cleanly when Central cannot be reached.

load "../lib/harness.bash"

setup_file() {
	export SUXEN_EXAMPLE_PUBLIC_READS=false
	suxen_start "$BATS_TEST_DIRNAME"
	cat >"$SUXEN_EXAMPLE_DATA/settings.xml" <<EOF
<settings><servers>
  <server><id>suxen-hosted</id><username>$SUXEN_EXAMPLE_USER</username><password>$SUXEN_EXAMPLE_PASSWORD</password></server>
  <server><id>suxen</id><username>$SUXEN_EXAMPLE_USER</username><password>$SUXEN_EXAMPLE_PASSWORD</password></server>
</servers></settings>
EOF
	cat >"$SUXEN_EXAMPLE_DATA/group-settings.xml" <<EOF
<settings>
  <servers>
    <server><id>suxen</id><username>$SUXEN_EXAMPLE_USER</username><password>$SUXEN_EXAMPLE_PASSWORD</password></server>
  </servers>
  <mirrors>
    <mirror><id>suxen</id><mirrorOf>*</mirrorOf><url>$SUXEN_URL/repository/maven</url></mirror>
  </mirrors>
</settings>
EOF
}
teardown_file() { suxen_stop; }

MAVEN_IMAGE="maven:3.9-eclipse-temurin-21"
CURL="curlimages/curl:8.11.1"

@test "an artifact deployed to maven-hosted resolves from it" {
	require_docker
	# Deploy is an HTTP PUT of the jar + pom to the coordinate path (what
	# mvn deploy / gradle publish do under the hood). The artifact itself
	# needs no upstream; Maven may resolve dependency:get from Central.
	printf 'hosted jar bytes\n' >"$SUXEN_EXAMPLE_DATA/demo-1.0.0.jar"
	cat >"$SUXEN_EXAMPLE_DATA/demo-1.0.0.pom" <<-'EOF'
		<project><modelVersion>4.0.0</modelVersion><groupId>com.example</groupId><artifactId>demo</artifactId><version>1.0.0</version></project>
	EOF
	for f in jar pom; do
		run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
			--upload-file "/work/demo-1.0.0.$f" \
			"$SUXEN_URL/repository/maven-hosted/com/example/demo/1.0.0/demo-1.0.0.$f"
		[ "$status" -eq 0 ]
	done

	run client "$MAVEN_IMAGE" mvn --batch-mode --no-transfer-progress \
		--settings /work/settings.xml \
		-Dmaven.repo.local=/work/m2-hosted \
		org.apache.maven.plugins:maven-dependency-plugin:3.8.1:get \
		-Dartifact=com.example:demo:1.0.0 \
		-DremoteRepositories="suxen-hosted::default::$SUXEN_URL/repository/maven-hosted" \
		-Dtransitive=false
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/m2-hosted/com/example/demo/1.0.0/demo-1.0.0.jar" ]
}

@test "cleanup retains a whole Maven version" {
	require_docker
	for version in 1.0.0 2.0.0; do
		for ext in pom jar jar.sha1; do
			printf 'retention-demo %s %s\n' "$version" "$ext" >"$SUXEN_EXAMPLE_DATA/retention-demo-$version.$ext"
			run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
				--upload-file "/work/retention-demo-$version.$ext" \
				"$SUXEN_URL/repository/maven-hosted/com/example/retention-demo/$version/retention-demo-$version.$ext"
			[ "$status" -eq 0 ]
		done
	done
	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		-H 'Content-Type: application/json' \
		-d '{"name":"maven-keep-one","repositories":["maven-hosted"],"criteria":[{"path":"maven.artifactId","op":"=","value":"retention-demo"}],"keepLast":1,"action":"delete","enabled":false}' \
		"$SUXEN_URL/api/v1/cleanup-policies"
	[ "$status" -eq 0 ]
	run client "$CURL" -fsS -X POST -H "Authorization: Bearer $SUXEN_TOKEN" \
		"$SUXEN_URL/api/v1/cleanup-policies/maven-keep-one/run?dryRun=false"
	[ "$status" -eq 0 ]
	for ext in pom jar jar.sha1; do
		run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
			-H "Authorization: Bearer $SUXEN_TOKEN" \
			"$SUXEN_URL/repository/maven-hosted/com/example/retention-demo/1.0.0/retention-demo-1.0.0.$ext"
		[ "$status" -eq 0 ]
		[ "$output" = 404 ]
		run client "$CURL" -fsS -o /dev/null -H "Authorization: Bearer $SUXEN_TOKEN" \
			"$SUXEN_URL/repository/maven-hosted/com/example/retention-demo/2.0.0/retention-demo-2.0.0.$ext"
		[ "$status" -eq 0 ]
	done
}

@test "mvn resolves an artifact through the suxen maven group" {
	require_docker
	# Authenticated read: dependency:get pulls commons-lang3 through the group,
	# whose proxy member caches it from Maven Central. The mirror forces
	# Maven's default Central repository through this group too; the isolated
	# local cache and provenance marker prove where the artifact came from.
	run client "$MAVEN_IMAGE" mvn --batch-mode --no-transfer-progress \
		--settings /work/group-settings.xml \
		-Dmaven.repo.local=/work/m2-group \
		org.apache.maven.plugins:maven-dependency-plugin:3.8.1:get \
		-Dartifact=org.apache.commons:commons-lang3:3.14.0 \
		-Dtransitive=false
	if [ "$status" -ne 0 ]; then
		maven_failure="$output"
		if ! client "$CURL" -fsSI --max-time 10 \
			https://repo.maven.apache.org/maven2/org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.pom \
			>/dev/null 2>&1; then
			skip "Maven Central not reachable"
		fi
		echo "$maven_failure" >&2
		false
	fi
	[ "$status" -eq 0 ]
	artifact_dir="$SUXEN_EXAMPLE_DATA/m2-group/org/apache/commons/commons-lang3/3.14.0"
	[ -f "$artifact_dir/commons-lang3-3.14.0.jar" ]
	grep -Fq 'commons-lang3-3.14.0.jar>suxen=' "$artifact_dir/_remote.repositories"
}
