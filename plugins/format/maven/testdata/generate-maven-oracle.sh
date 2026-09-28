#!/usr/bin/env sh
set -eu

: "${JAVA_HOME:?set JAVA_HOME to a Java 21 installation}"
: "${MAVEN_HOME:?set MAVEN_HOME to a Maven 3.9.16 installation}"

case "$("$JAVA_HOME/bin/java" -version 2>&1 | head -n 1)" in
  *'"21.'*) ;;
  *) echo 'generator requires Java 21' >&2; exit 1 ;;
esac

jar="$MAVEN_HOME/lib/maven-artifact-3.9.16.jar"
directory=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

"$JAVA_HOME/bin/javac" -cp "$jar" -d "$temporary" "$directory/VersionOracle.java"
"$JAVA_HOME/bin/java" -cp "$jar:$temporary" VersionOracle "$directory/maven-3.9.16-java21-versions.txt"
