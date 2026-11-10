#!/bin/sh
# Archive Center 2 — fresh install for Linux, macOS and Termux.
#   curl -fsSL https://raw.githubusercontent.com/Flazer31/archive-center-2/main/install.sh | sh
# It downloads the release package for this machine, checks its SHA-256,
# unpacks it under ~/archive-center2/ac2-<version> and starts it. A folder of
# that version that already exists is never written into.
# The whole script is one function, called on the last line, so a download cut
# off inside it runs nothing.
set -eu

main() {
    fail() { printf 'AC2 설치 오류: %s\n' "$*" >&2; exit 1; }
    usage() { printf '%s\n' '사용법: sh install.sh [--version X.Y.Z] [--repo OWNER/REPO]'; }
    has_cmd() { command -v "$1" >/dev/null 2>&1; }

    repo=Flazer31/archive-center-2
    version=
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --repo) [ "$#" -ge 2 ] || fail '--repo 값이 필요합니다.'; repo=$2; shift 2 ;;
            --version) [ "$#" -ge 2 ] || fail '--version 값이 필요합니다.'; version=$2; shift 2 ;;
            -h|--help) usage; exit 0 ;;
            *) usage >&2; fail "알 수 없는 옵션: $1" ;;
        esac
    done
    LC_ALL=C
    export LC_ALL
    case "$repo$version" in *'
'*) fail '저장소와 버전은 각각 한 줄이어야 합니다.' ;; esac
    printf '%s\n' "$repo" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*$' ||
        fail '--repo에는 GitHub 저장소의 OWNER/REPO를 입력해 주세요.'
    [ -n "${HOME:-}" ] || fail 'HOME 환경 변수가 필요합니다.'
    install_root="$HOME/archive-center2"

    # The version's form, and a folder of that version already there, stop the
    # install. With --version this runs first: before any tool is installed
    # and before anything is created.
    check_version() {
        printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
            fail '버전은 X.Y.Z 형식이어야 합니다(예: 1.0.0).'
        install_dir="$install_root/ac2-$version"
        if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
            fail "이 버전이 이미 설치되어 있습니다. 덮어쓰지 않습니다: $install_dir (시작: sh $install_dir/01_AC2_start.sh)"
        fi
    }
    [ -z "$version" ] || check_version

    os=$(uname -s)
    arch=$(uname -m)
    case "${PREFIX:-}" in
        */com.termux/*)
            case "$os/$arch" in Linux/aarch64|Linux/arm64) platform=android-arm64 ;;
                *) fail "지원하지 않는 Termux 플랫폼입니다: $os/$arch" ;;
            esac ;;
        *)
            case "$os/$arch" in
                Linux/x86_64|Linux/amd64) platform=linux-amd64 ;;
                Linux/aarch64|Linux/arm64) platform=linux-arm64 ;;
                Darwin/x86_64) platform=darwin-amd64 ;;
                Darwin/arm64|Darwin/aarch64) platform=darwin-arm64 ;;
                *) fail "지원하지 않는 플랫폼입니다: $os/$arch" ;;
            esac ;;
    esac

    as_root() {
        if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi
    }

    # need: the named tools, installed when missing. curl comes first (to
    # read the latest version); unzip and a checksum tool only once the
    # version is known not to be installed yet.
    need() {
        want=$*
        set --
        for tool in $want; do
            case "$tool" in
                sha) has_cmd sha256sum || has_cmd shasum || set -- "$@" coreutils ;;
                *) has_cmd "$tool" || set -- "$@" "$tool" ;;
            esac
        done
        [ "$#" -gt 0 ] || return 0
        printf 'AC2 설치에 필요한 도구를 준비합니다: %s\n' "$*"
        case "$platform" in
            android-arm64) pkg install -y "$@" ;;
            darwin-*) fail "다음 도구를 설치한 뒤 다시 실행해 주세요: $*" ;;
            *)
                if has_cmd apt-get; then
                    as_root apt-get update
                    as_root apt-get install -y "$@"
                elif has_cmd dnf; then as_root dnf install -y "$@"
                elif has_cmd yum; then as_root yum install -y "$@"
                elif has_cmd zypper; then as_root zypper --non-interactive install "$@"
                elif has_cmd pacman; then as_root pacman -S --needed --noconfirm "$@"
                else fail "다음 도구를 설치한 뒤 다시 실행해 주세요: $*"
                fi ;;
        esac
    }
    need curl

    umask 077
    work=$(mktemp -d "$HOME/.ac2-install.XXXXXX")
    claimed=
    # the work folder goes; so does an install folder this run made and did not finish
    trap 'rm -rf -- "$work"; [ -z "$claimed" ] || rm -rf -- "$claimed"' 0
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM

    # no --version: the latest release of the repository
    if [ -z "$version" ]; then
        curl -fsSL "https://api.github.com/repos/$repo/releases/latest" -o "$work/latest.json" ||
            fail '최신 버전을 확인하지 못했습니다. 잠시 뒤 다시 하거나 --version X.Y.Z를 붙여 주세요.'
        version=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\([0-9][0-9.]*\)".*/\1/p' "$work/latest.json" | sed -n 1p)
        [ -n "$version" ] || fail '최신 버전 번호를 읽지 못했습니다. --version X.Y.Z를 붙여 주세요.'
        check_version
    fi
    need unzip sha

    asset="ac2-$version-$platform.zip"
    base_url="https://github.com/$repo/releases/download/v$version"
    printf 'Archive Center 2 %s (%s)을 받습니다.\n' "$version" "$platform"
    curl -fsSL "$base_url/SHA256SUMS.txt" -o "$work/SHA256SUMS.txt" ||
        fail "체크섬 목록을 받지 못했습니다: $base_url/SHA256SUMS.txt"
    curl -fsSL "$base_url/$asset" -o "$work/$asset" || fail "패키지를 받지 못했습니다: $base_url/$asset"
    expected=$(awk -v asset="$asset" '
        { sub(/\r$/, "") }
        NF == 2 && ($2 == asset || $2 == "*" asset) && length($1) == 64 && $1 !~ /[^0-9a-fA-F]/ {
            count++; hash = tolower($1)
        }
        END { if (count == 1) print hash; else exit 1 }
    ' "$work/SHA256SUMS.txt") || fail "체크섬 목록에서 $asset 항목을 확인할 수 없습니다."
    if has_cmd sha256sum; then actual=$(sha256sum < "$work/$asset")
    else actual=$(shasum -a 256 < "$work/$asset")
    fi
    [ "${actual%% *}" = "$expected" ] || fail "$asset SHA-256 검증에 실패했습니다."

    mkdir -p -- "$install_root"
    # mkdir makes the folder or fails when it is there: a folder that appeared
    # meanwhile (another install running) is never written into
    # (signals wait while the folder is made and noted as this run's own)
    trap '' HUP INT TERM
    mkdir -- "$install_dir" 2>/dev/null || fail "설치 경로가 이미 존재합니다. 덮어쓰지 않습니다: $install_dir"
    claimed=$install_dir
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM
    unzip -q "$work/$asset" -d "$install_dir" || fail '패키지를 풀지 못했습니다.'
    [ -f "$install_dir/01_AC2_start.sh" ] || fail '패키지에서 01_AC2_start.sh를 찾을 수 없습니다.'
    claimed=
    rm -rf -- "$work"
    trap - 0 HUP INT TERM

    printf 'AC2 설치 경로: %s\n' "$install_dir"
    printf '%s\n' '다음부터는 이 폴더의 01_AC2_start.sh로 시작합니다. RisuAI에는 이 폴더의 archive-center-2.js를 가져오세요.'
    # run as "curl … | sh" the script itself is on stdin: give the start
    # script the terminal, so it can ask (a package manager's password)
    case "$0" in
        *install.sh) ;;
        *)
            if [ ! -t 0 ] && (: < /dev/tty) 2>/dev/null; then
                exec sh "$install_dir/01_AC2_start.sh" < /dev/tty
            fi ;;
    esac
    exec sh "$install_dir/01_AC2_start.sh"
}

main "$@"
