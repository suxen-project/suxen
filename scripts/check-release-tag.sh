#!/usr/bin/env bash
set -euo pipefail

repository="${1:?usage: check-release-tag.sh OWNER/REPOSITORY TAG [BRANCH]}"
tag="${2:?usage: check-release-tag.sh OWNER/REPOSITORY TAG [BRANCH]}"
branch="${3:-main}"
gh_bin="${GH_BIN:-gh}"

tag_ref="$("${gh_bin}" api "repos/${repository}/git/ref/tags/${tag}")"
if [[ "$(jq -r '.object.type' <<<"${tag_ref}")" != tag ]]; then
	printf 'release tag %s must be annotated\n' "${tag}" >&2
	exit 1
fi

tag_object_sha="$(jq -r '.object.sha' <<<"${tag_ref}")"
tag_object="$("${gh_bin}" api "repos/${repository}/git/tags/${tag_object_sha}")"
if [[ "$(jq -r '.verification.verified' <<<"${tag_object}")" != true ]]; then
	printf 'release tag %s is not GitHub-verified\n' "${tag}" >&2
	exit 1
fi
if [[ "$(jq -r '.object.type' <<<"${tag_object}")" != commit ]]; then
	printf 'release tag %s does not resolve directly to a commit\n' "${tag}" >&2
	exit 1
fi

tag_commit="$(jq -r '.object.sha' <<<"${tag_object}")"
branch_commit="$("${gh_bin}" api "repos/${repository}/git/ref/heads/${branch}" --jq '.object.sha')"
if [[ "${tag_commit}" != "${branch_commit}" ]]; then
	printf 'release tag %s resolves to %s, but %s resolves to %s\n' \
		"${tag}" "${tag_commit}" "${branch}" "${branch_commit}" >&2
	exit 1
fi

printf 'release tag %s is verified and resolves to %s at %s\n' \
	"${tag}" "${tag_commit}" "${branch}"
