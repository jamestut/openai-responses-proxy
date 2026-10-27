#!/bin/sh

set -eu

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd -P)
cd "$repo_root"

build_time=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
version=dev

if command -v git >/dev/null 2>&1; then
	if git_root=$(git -C "$repo_root" rev-parse --show-toplevel 2>/dev/null) && [ "$git_root" = "$repo_root" ]; then
		if version=$(git -C "$repo_root" describe --tags --exact-match HEAD 2>/dev/null); then
			git_metadata=1
		elif version=$(git -C "$repo_root" rev-parse --short HEAD 2>/dev/null); then
			git_metadata=1
		else
			version=dev
			git_metadata=0
		fi
		if [ "$git_metadata" -eq 1 ]; then
			if status=$(git -C "$repo_root" status --porcelain --untracked-files=all 2>/dev/null); then
				if [ -n "$status" ]; then
					version="${version}-dev"
				fi
			else
				version=dev
			fi
		fi
	fi
fi

ldflags="-s -w -X main.version=$version -X main.buildTime=$build_time"

mkdir -p bin
go build -trimpath -ldflags "$ldflags" -o "$repo_root/bin/responses-proxy" ./cmd/responses-proxy
