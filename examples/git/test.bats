#!/usr/bin/env bats
# Boots the example, then clones through both git proxies with a real git client.
# Both proxies fetch from public upstreams, so these tests need network egress;
# each skips cleanly when its upstream cannot be reached.

load "../lib/harness.bash"

setup_file() { export SUXEN_EXAMPLE_PUBLIC_READS=false; suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

GIT_IMAGE="alpine/git:2.45.2"
ARCH_IMAGE="archlinux:base-devel@sha256:70d777aaeb45befc04150df137c4d7c1b5042be442b4c904c38c6f6880bb7844"

@test "shallow-clone a GitHub repository through the git proxy" {
	require_docker
	# The alpine/git entrypoint is `git`, so pass subcommands directly. A CI
	# checkout is a --depth 1 clone, which is exactly what the snapshot proxy
	# serves. The URL expands in the test shell.
	run client "$GIT_IMAGE" clone --depth 1 \
		"http://$SUXEN_EXAMPLE_USER:$SUXEN_EXAMPLE_PASSWORD@${SUXEN_URL#http://}/repository/github/octocat/Hello-World.git" /work/hw
	if [ "$status" -ne 0 ]; then
		clone_failure="$output"
		if ! client "$GIT_IMAGE" ls-remote https://github.com/octocat/Hello-World.git HEAD \
			>/dev/null 2>&1; then
			skip "GitHub not reachable"
		fi
		echo "$clone_failure" >&2
		false
	fi
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/hw/README" ]
}

@test "clone an AUR package recipe through the git proxy" {
	require_docker
	# AUR packages are single-branch, so the one snapshot suxen serves clones
	# with --single-branch.
	run client "$GIT_IMAGE" clone --single-branch \
		"http://$SUXEN_EXAMPLE_USER:$SUXEN_EXAMPLE_PASSWORD@${SUXEN_URL#http://}/repository/aur/yay.git" /work/yay
	if [ "$status" -ne 0 ]; then
		clone_failure="$output"
		if ! client "$GIT_IMAGE" ls-remote https://aur.archlinux.org/yay.git HEAD \
			>/dev/null 2>&1; then
			skip "AUR upstream not reachable"
		fi
		echo "$clone_failure" >&2
		false
	fi
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/yay/PKGBUILD" ]

	# The Arch toolchain consumes the proxied recipe: makepkg parses the PKGBUILD
	# (a full build is out of scope). makepkg refuses to run as root, so this
	# relies on client() running as the host user.
	run client "$ARCH_IMAGE" bash -c 'cd /work/yay && makepkg --printsrcinfo'
	[ "$status" -eq 0 ]
	[[ "$output" == *"pkgbase = yay"* ]]
}
