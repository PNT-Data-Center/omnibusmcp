#!/usr/bin/env bash
# OmnibusMCP installer: downloads a release binary for this machine, checks
# it against the release's SHA256SUMS and installs it.
#
#   curl -fsSL https://github.com/PNT-Data-Center/omnibusmcp/releases/latest/download/install.sh | sudo bash
#   curl -fsSL .../install.sh | sudo bash -s -- --version v0.4.0
#   curl -fsSL .../install.sh | sudo bash -s -- --install --listen 192.0.2.10:8765
#
# Options:
#   --version vX.Y.Z   install this release instead of the latest
#   --install [ARGS]   then run "omnibusmcp install ARGS" (set up the systemd
#                      service) unless it is already installed
#   -h, --help         show this help
#
# Environment:
#   GH_TOKEN / GITHUB_TOKEN   token for a private repository (GitHub API)
#   OMNIBUSMCP_REPO           owner/name on GitHub (default PNT-Data-Center/omnibusmcp)
#   OMNIBUSMCP_BASE_URL       download base for public releases, without the
#                             tag part (default https://github.com/$OMNIBUSMCP_REPO/releases)
#   OMNIBUSMCP_BIN_DIR        install directory (default /usr/local/bin)
#
# Everything runs inside main(), called on the last line: a download cut
# short never executes a partial script.

set -euo pipefail

main() {
	local repo="${OMNIBUSMCP_REPO:-PNT-Data-Center/omnibusmcp}"
	local base="${OMNIBUSMCP_BASE_URL:-https://github.com/$repo/releases}"
	local bin_dir="${OMNIBUSMCP_BIN_DIR:-/usr/local/bin}"
	local token="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
	local version="" run_install=0
	local -a install_args=()

	while [ $# -gt 0 ]; do
		case "$1" in
		--version)
			[ $# -ge 2 ] || die "--version needs a value, e.g. v0.4.0"
			version="$2"
			shift 2
			;;
		--install)
			run_install=1
			shift
			install_args=("$@")
			break
			;;
		-h | --help)
			sed -n '2,24p' "${BASH_SOURCE[0]:-/dev/null}" 2>/dev/null | sed 's/^# \{0,1\}//' ||
				echo "see https://github.com/$repo"
			return 0
			;;
		*) die "unknown option: $1 (see --help)" ;;
		esac
	done
	if [ -n "$version" ] && ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
		die "invalid version \"$version\" (expected e.g. v0.4.0)"
	fi

	[ "$(uname -s)" = "Linux" ] || die "OmnibusMCP runs on Linux only"
	local arch
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture $(uname -m) (supported: amd64, arm64)" ;;
	esac
	need curl
	need sha256sum
	mkdir -p "$bin_dir" 2>/dev/null || true
	[ -w "$bin_dir" ] || die "cannot write to $bin_dir: run as root (curl ... | sudo bash)"

	local asset="omnibusmcp-linux-$arch" tmp
	tmp="$(mktemp -d)"
	# shellcheck disable=SC2064 # expand now: tmp is local to main
	trap "rm -rf '$tmp'" EXIT

	say "downloading $asset (${version:-latest release})"
	if [ -n "$token" ]; then
		fetch_api "$repo" "$version" "$token" "$asset" "$tmp/$asset"
		fetch_api "$repo" "$version" "$token" SHA256SUMS "$tmp/SHA256SUMS"
	else
		local url="$base/latest/download"
		[ -n "$version" ] && url="$base/download/$version"
		fetch "$url/$asset" "$tmp/$asset"
		fetch "$url/SHA256SUMS" "$tmp/SHA256SUMS"
	fi

	(cd "$tmp" && grep -E "  $asset\$" SHA256SUMS | sha256sum -c --status) ||
		die "checksum of $asset does not match SHA256SUMS: download corrupted, nothing installed"
	chmod 0755 "$tmp/$asset"
	"$tmp/$asset" version >/dev/null 2>&1 || die "the downloaded binary does not run on this machine"

	# Replace atomically: a running service keeps its old binary until restart.
	install -m 0755 "$tmp/$asset" "$bin_dir/.omnibusmcp.new"
	mv -f "$bin_dir/.omnibusmcp.new" "$bin_dir/omnibusmcp"
	say "installed $("$bin_dir/omnibusmcp" version | head -n1) to $bin_dir/omnibusmcp"

	if [ "$bin_dir" = /usr/local/bin ] && command -v systemctl >/dev/null 2>&1 &&
		systemctl is-active --quiet omnibusmcp 2>/dev/null; then
		systemctl restart omnibusmcp
		say "omnibusmcp.service restarted with the new version"
	elif [ "$run_install" = 1 ]; then
		if systemctl list-unit-files omnibusmcp.service 2>/dev/null | grep -q omnibusmcp; then
			say "omnibusmcp.service already installed; skipping 'omnibusmcp install'"
		else
			"$bin_dir/omnibusmcp" install "${install_args[@]}"
		fi
	else
		say "next: sudo omnibusmcp install   (see: omnibusmcp help)"
	fi
}

# fetch downloads a public URL, failing on HTTP errors.
fetch() {
	curl -fsSL --retry 2 -o "$2" "$1" || die "download failed: $1"
}

# fetch_api downloads a release asset of a private repository through the
# GitHub API: find the asset's API URL in the release JSON, then request it
# as application/octet-stream.
fetch_api() {
	local repo="$1" version="$2" token="$3" name="$4" out="$5" release json asset_url
	release="https://api.github.com/repos/$repo/releases/latest"
	[ -n "$version" ] && release="https://api.github.com/repos/$repo/releases/tags/$version"
	json="$(curl -fsSL -H "Authorization: Bearer $token" -H "Accept: application/vnd.github+json" "$release")" ||
		die "cannot read release ${version:-latest} of $repo (token valid? repository name correct?)"
	# Each asset object lists "url" (API URL of the asset) before "name".
	asset_url="$(printf '%s\n' "$json" | tr ',' '\n' | awk -v n="\"$name\"" '
		/"url": *"https:\/\/api\.github\.com\/repos\/.*\/releases\/assets\// { u = $0; sub(/.*"url": *"/, "", u); sub(/".*/, "", u) }
		/"name":/ { v = $0; sub(/.*"name": */, "", v); sub(/[ }\]]*$/, "", v); if (v == n && u != "") { print u; exit } }')"
	[ -n "$asset_url" ] || die "release ${version:-latest} of $repo has no asset $name"
	curl -fsSL --retry 2 -H "Authorization: Bearer $token" -H "Accept: application/octet-stream" -o "$out" "$asset_url" ||
		die "download failed: $name"
}

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
say() { printf 'omnibusmcp-install: %s\n' "$*"; }
die() {
	printf 'omnibusmcp-install: error: %s\n' "$*" >&2
	exit 1
}

main "$@"
