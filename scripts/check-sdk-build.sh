#!/bin/sh
# Exercise suxen-build path handling without downloading modules or compiling Go.
set -eu

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
real_go="$(command -v go)"
caller_dir="$test_dir/caller with space"
quoted_plugin='plugin "quoted"'

mkdir -p "$test_dir/bin" "$caller_dir/source" "$caller_dir/plugin" "$caller_dir/$quoted_plugin"
printf 'module github.com/suxen-project/suxen\n\ngo 1.26.8\n' > "$caller_dir/source/go.mod"
cat > "$test_dir/bin/go" <<'EOF'
#!/bin/sh
set -eu
case "$1" in
mod)
  [ "$2" = tidy ]
  cp go.mod "$SDK_TEST_MODFILE"
  ;;
build)
  while [ "$#" -gt 0 ]; do
    if [ "$1" = -o ]; then
      printf '%s\n' "$2" > "$SDK_TEST_OUTPUT_ARG"
      if [ "$2" != /out/suxen ]; then
        printf 'built\n' > "$2"
      fi
      exit 0
    fi
    shift
  done
  exit 1
  ;;
*) exit 1 ;;
esac
EOF
cat > "$test_dir/bin/mkdir" <<'EOF'
#!/bin/sh
set -eu
if [ "$#" -eq 2 ] && [ "$1" = -p ] && [ "$2" = /out ]; then
  exit 0
fi
exec /bin/mkdir "$@"
EOF
chmod +x "$test_dir/bin/go" "$test_dir/bin/mkdir"

(
  cd "$caller_dir"
  export PATH="$test_dir/bin:$PATH"
  export SDK_TEST_MODFILE="$test_dir/generated.mod"
  export SDK_TEST_OUTPUT_ARG="$test_dir/output-arg"

  SUXEN_SOURCE=./source "$root_dir/sdk/suxen-build" \
    --no-defaults --plugin example.com/plugin=./plugin --output relative/suxen
  [ "$(cat "$test_dir/output-arg")" = "$caller_dir/relative/suxen" ]
  [ "$(cat relative/suxen)" = built ]
  grep -Fqx "replace github.com/suxen-project/suxen => \"$caller_dir/./source\"" "$test_dir/generated.mod"
  grep -Fqx "replace example.com/plugin => \"$caller_dir/./plugin\"" "$test_dir/generated.mod"
  "$real_go" mod edit -json "$test_dir/generated.mod" >/dev/null

  SUXEN_SOURCE=./source "$root_dir/sdk/suxen-build" \
    --no-defaults --plugin "example.com/plugin=./$quoted_plugin" --output relative/quoted
  expected_quoted='replace example.com/plugin => "'"$caller_dir"'/./plugin \"quoted\""'
  grep -Fqx "$expected_quoted" "$test_dir/generated.mod"
  "$real_go" mod edit -json "$test_dir/generated.mod" >/dev/null

  SUXEN_SOURCE="$caller_dir/source" "$root_dir/sdk/suxen-build" \
    --no-defaults --plugin "example.com/plugin=$caller_dir/plugin" \
    --output "$test_dir/absolute/suxen"
  [ "$(cat "$test_dir/output-arg")" = "$test_dir/absolute/suxen" ]
  [ "$(cat "$test_dir/absolute/suxen")" = built ]
  grep -Fqx "replace example.com/plugin => \"$caller_dir/plugin\"" "$test_dir/generated.mod"

  SUXEN_SOURCE=./source "$root_dir/sdk/suxen-build" --no-defaults
  [ "$(cat "$test_dir/output-arg")" = /out/suxen ]

  if SUXEN_SOURCE=./source "$root_dir/sdk/suxen-build" --output '' >/dev/null 2>&1; then
    echo 'suxen-build accepted an empty output path' >&2
    exit 1
  fi
  if SUXEN_SOURCE=./source "$root_dir/sdk/suxen-build" --plugin example.com/plugin= >/dev/null 2>&1; then
    echo 'suxen-build accepted an empty local plugin path' >&2
    exit 1
  fi
  if SUXEN_SOURCE=./source "$root_dir/sdk/suxen-build" \
      --plugin 'example.com/plugin=./plugin\backslash' >/dev/null 2>&1; then
    echo 'suxen-build accepted a Go-incompatible backslash replacement path' >&2
    exit 1
  fi
)
