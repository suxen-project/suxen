#!/usr/bin/env bats
# Boots the example, then exercises both halves of the go group:
#  1. a module downloaded through the proxy member (needs network; skips offline)
#  2. a module published to the hosted member and resolved through the group
#     (hermetic — no upstream)

load "../lib/harness.bash"

setup_file() {
	export SUXEN_EXAMPLE_PUBLIC_READS=false
	suxen_start "$BATS_TEST_DIRNAME"
	suxen_enable_tls
}
teardown_file() { suxen_stop; }

GO_IMAGE="golang:1.25"
CURL="curlimages/curl:8.11.1"

@test "go mod download resolves through the suxen group proxy" {
	require_docker
	run client "$GO_IMAGE" sh -c '
		set -e
		cat > /work/goauth <<EOF
#!/bin/sh
printf "%s\\n\\nAuthorization: Bearer %s\\n\\n" "$SUXEN_URL" "$SUXEN_TOKEN"
EOF
		chmod 700 /work/goauth
		export GOAUTH=/work/goauth
		export GOPROXY="$SUXEN_URL/repository/go" GOSUMDB=off
		export GOMODCACHE=/work/gomodcache GOFLAGS=-modcacherw
		go mod download -x rsc.io/quote@v1.5.2
	'
	if [ "$status" -ne 0 ]; then
		skip "upstream module proxy not reachable (offline?): ${output}"
	fi
	[ "$status" -eq 0 ]
	[ -d "$SUXEN_EXAMPLE_DATA/gomodcache/cache/download/rsc.io/quote" ]
}

@test "a module published to go-hosted resolves through the group" {
	require_docker
	local d="$SUXEN_EXAMPLE_DATA"
	mkdir -p "$d/mk"
	cat >"$d/mk/go.mod" <<-EOF
		module mk
		go 1.21
	EOF
	# A stdlib-only helper that writes a module's .info/.mod/.zip (the go module
	# zip layout is <module>@<version>/<path>), so no network is needed to build
	# a publishable module.
	cat >"$d/mk/main.go" <<-'GO'
		package main

		import (
			"archive/zip"
			"fmt"
			"os"
			"path/filepath"
		)

		func main() {
			module, version, outDir := os.Args[1], os.Args[2], os.Args[3]
			must(os.MkdirAll(outDir, 0o755))
			gomod := fmt.Sprintf("module %s\n\ngo 1.21\n", module)
			must(os.WriteFile(filepath.Join(outDir, version+".mod"), []byte(gomod), 0o644))
			must(os.WriteFile(filepath.Join(outDir, version+".info"),
				[]byte(fmt.Sprintf("{\"Version\":%q,\"Time\":\"2026-01-01T00:00:00Z\"}", version)), 0o644))
			zf, err := os.Create(filepath.Join(outDir, version+".zip"))
			must(err)
			defer zf.Close()
			zw := zip.NewWriter(zf)
			prefix := module + "@" + version + "/"
			add := func(name, content string) {
				w, err := zw.Create(prefix + name)
				must(err)
				_, err = w.Write([]byte(content))
				must(err)
			}
			add("go.mod", gomod)
			add("hello.go", "package hello\n\n// Hello greets.\nfunc Hello() string { return \"hi\" }\n")
			must(zw.Close())
		}

		func must(err error) {
			if err != nil {
				panic(err)
			}
		}
	GO

	run client "$GO_IMAGE" sh -c 'cd /work/mk && GOCACHE=/work/gocache go run . example.com/hello v1.0.0 /work/mod'
	[ "$status" -eq 0 ]

	# Publish the module files to the hosted member (needs write).
	for ext in info mod zip; do
		run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
			--upload-file "/work/mod/v1.0.0.$ext" \
			"$SUXEN_URL/repository/go-hosted/example.com/hello/@v/v1.0.0.$ext"
		[ "$status" -eq 0 ]
	done

	# The group serves the hosted module alongside proxied ones. No network.
	run client "$GO_IMAGE" sh -c '
		set -e
		cat > /work/goauth <<EOF
#!/bin/sh
printf "%s\\n\\nAuthorization: Bearer %s\\n\\n" "$SUXEN_URL" "$SUXEN_TOKEN"
EOF
		chmod 700 /work/goauth
		export GOAUTH=/work/goauth
		export GOPROXY="$SUXEN_URL/repository/go" GOSUMDB=off
		export GOMODCACHE=/work/gmc-hosted GOFLAGS=-modcacherw GOCACHE=/work/gocache
		go mod download -x example.com/hello@v1.0.0
	'
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/gmc-hosted/cache/download/example.com/hello/@v/v1.0.0.zip" ]

	# Publish a second complete version, then retain one module version. Cleanup
	# must keep all three files of v1.1.0 and remove all three of v1.0.0.
	run client "$GO_IMAGE" sh -c 'cd /work/mk && GOCACHE=/work/gocache go run . example.com/hello v1.1.0 /work/mod'
	[ "$status" -eq 0 ]
	for ext in info mod zip; do
		run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
			--upload-file "/work/mod/v1.1.0.$ext" \
			"$SUXEN_URL/repository/go-hosted/example.com/hello/@v/v1.1.0.$ext"
		[ "$status" -eq 0 ]
	done
	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		-H 'Content-Type: application/json' \
		-d '{"name":"go-keep-one","repositories":["go-hosted"],"criteria":[{"path":"go.module","op":"=","value":"example.com/hello"}],"keepLast":1,"action":"delete","enabled":false}' \
		"$SUXEN_URL/api/v1/cleanup-policies"
	[ "$status" -eq 0 ]
	run client "$CURL" -fsS -X POST -H "Authorization: Bearer $SUXEN_TOKEN" \
		"$SUXEN_URL/api/v1/cleanup-policies/go-keep-one/run?dryRun=false"
	[ "$status" -eq 0 ]
	for ext in info mod zip; do
		run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
			-H "Authorization: Bearer $SUXEN_TOKEN" \
			"$SUXEN_URL/repository/go-hosted/example.com/hello/@v/v1.0.0.$ext"
		[ "$status" -eq 0 ]
		[ "$output" = 404 ]
		run client "$CURL" -fsS -o /dev/null \
			-H "Authorization: Bearer $SUXEN_TOKEN" \
			"$SUXEN_URL/repository/go-hosted/example.com/hello/@v/v1.1.0.$ext"
		[ "$status" -eq 0 ]
	done
}
