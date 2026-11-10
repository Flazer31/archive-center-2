#!/bin/sh
set -eu
banner='Archive Center 2 백엔드를 시작합니다. 이 터미널을 닫으면 백엔드가 꺼집니다.'
printf '%s\n' "$banner"
package_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$package_dir"
umask 077

# Match the backend's last explicit -data argument without consuming argv.
# Same default as the backend's shared data folder (datapath.Resolve); logs only.
if [ -n "${AC2_DATA_DIR:-}" ]; then
    log_data=$AC2_DATA_DIR
elif [ "$(uname -s)" = Darwin ]; then
    log_data="${HOME}/Library/Application Support/ArchiveCenter2/data"
else
    log_data="${XDG_DATA_HOME:-$HOME/.local/share}/archive-center2/data"
fi
expect_data=false
for argument do
    if "$expect_data"; then log_data=$argument; expect_data=false; continue; fi
    case "$argument" in
        -data|--data) expect_data=true ;;
        -data=*|--data=*) log_data=${argument#*=} ;;
    esac
done
case "$log_data" in /*) ;; *) log_data="$package_dir/$log_data" ;; esac
log_dir="$log_data/logs"
if ! (mkdir -p -- "$log_dir" && chmod 700 "$log_dir" && : > "$log_dir/.start-write-$$") 2>/dev/null; then
    log_dir="${TMPDIR:-/tmp}/ac2-start-logs"
    mkdir -p -- "$log_dir"
    chmod 700 "$log_dir"
else
    rm -f -- "$log_dir/.start-write-$$"
fi
log_dir=$(CDPATH= cd -- "$log_dir" && pwd -P)
AC2_START_LOG="$log_dir/start-$(date '+%Y%m%d-%H%M%S')-$$.log"
export AC2_START_LOG
: > "$AC2_START_LOG"
chmod 600 "$AC2_START_LOG"
printf '%s\n' "$banner" >> "$AC2_START_LOG"

log_line() { printf '%s\n' "$*"; printf '%s\n' "$*" >> "$AC2_START_LOG"; }
# Only this startup log namespace is eligible for retention.
retained=0
for old_log in "$log_dir"/start-*.log; do
    if [ -f "$old_log" ]; then printf '%s\n' "${old_log##*/}"; fi
done | LC_ALL=C sort -r | while IFS= read -r old_log; do
    case "$old_log" in start-*.log) ;; *) continue ;; esac
    retained=$((retained + 1))
    if [ "$retained" -gt 10 ]; then rm -f -- "$log_dir/$old_log"; fi
done
log_line "시작 로그: $AC2_START_LOG"
stage='시작 준비'
command_pid=
heartbeat_pid=
output_pid=
output_dir=
repo_setup=
exec 3<&0

stop_helpers() {
    if [ -n "$heartbeat_pid" ]; then
        kill "$heartbeat_pid" 2>/dev/null || :
        wait "$heartbeat_pid" 2>/dev/null || :
        heartbeat_pid=
    fi
    if [ -n "$output_pid" ]; then
        wait "$output_pid" 2>/dev/null || :
        output_pid=
    fi
    if [ -n "$output_dir" ]; then rm -rf -- "$output_dir"; output_dir=; fi
}
on_signal() {
    signal_code=$1
    trap '' HUP INT TERM
    if [ -n "$command_pid" ]; then
        kill -TERM "$command_pid" 2>/dev/null || :
        wait "$command_pid" 2>/dev/null || :
        command_pid=
    fi
    exit "$signal_code"
}
finish() {
    result=$?
    trap - 0
    stop_helpers
    if [ -n "$repo_setup" ]; then rm -f -- "$repo_setup"; fi
    if [ "$result" -eq 0 ]; then
        log_line '정상 종료됨 (종료 코드: 0)'
    elif [ "$result" -eq 130 ] || [ "$result" -eq 143 ] || [ "$result" -eq 129 ]; then
        log_line "종료 요청으로 중단됨: $stage (종료 코드: $result)"
    else
        log_line "오류: $stage 단계에서 멈췄습니다 (종료 코드: $result). 위의 원래 오류를 확인해 주세요."
        case "$stage" in
            *MariaDB*|*Homebrew*) log_line '인터넷 연결과 설치 권한을 확인해 주세요. MariaDB 설치 도구의 오류가 위에 표시됩니다.' ;;
            *실행*|*업데이트*) log_line '압축을 모두 풀었는지, 실행 파일이 있는지와 실행 권한·백신 차단 여부를 확인해 주세요. 이미 실행 중인 시작 창도 확인해 주세요.' ;;
            *) log_line '폴더 쓰기 권한과 남은 디스크 공간을 확인해 주세요.' ;;
        esac
    fi
    log_line "로그 파일: $AC2_START_LOG"
    exit "$result"
}
trap finish 0
trap 'on_signal 129' HUP
trap 'on_signal 130' INT
trap 'on_signal 143' TERM

# Package-manager output is shell-owned. The Go runner writes its own streams
# to AC2_START_LOG; never tee it here (including the first-use access token).
start_heartbeat() {
    (
        sleep_pid=
        trap 'if [ -n "$sleep_pid" ]; then kill "$sleep_pid" 2>/dev/null || :; fi; exit 0' HUP INT TERM
        while :; do
            sleep 15 & sleep_pid=$!
            wait "$sleep_pid" || exit 0
            log_line "아직 진행 중입니다: $stage (설치 또는 백신 검사 등으로 늦어질 수 있어요)."
        done
    ) &
    heartbeat_pid=$!
}
run_logged() {
    output_dir=$(mktemp -d "${TMPDIR:-/tmp}/ac2-start-output.XXXXXX")
    mkfifo "$output_dir/output"
    tee -a "$AC2_START_LOG" < "$output_dir/output" &
    output_pid=$!
    start_heartbeat
    "$@" <&3 > "$output_dir/output" 2>&1 &
    command_pid=$!
    command_result=0
    wait "$command_pid" || command_result=$?
    command_pid=
    stop_helpers
    return "$command_result"
}
as_root() {
    if [ "$(id -u)" = 0 ]; then run_logged "$@"; else run_logged sudo "$@"; fi
}

stage='MariaDB 설치 확인'
log_line '[준비] 시스템 MariaDB 설치를 확인합니다.'

# MariaDB is a separate system dependency on POSIX, just as in AC1. Only
# the selected shared data directory is initialized or started by -managed.
if [ -n "${PREFIX:-}" ] && command -v pkg >/dev/null 2>&1; then
    if [ -z "${AC2_DNS_SERVERS:-}" ]; then
        if [ -r "$PREFIX/etc/resolv.conf" ]; then
            AC2_DNS_SERVERS=$(awk '$1 == "nameserver" && NF >= 2 { sub(/\r$/, "", $2); printf "%s%s", separator, $2; separator = "," }' "$PREFIX/etc/resolv.conf" 2>/dev/null) || AC2_DNS_SERVERS=
        fi
        AC2_DNS_SERVERS=${AC2_DNS_SERVERS:-1.1.1.1:53,8.8.8.8:53}
    fi
    export AC2_DNS_SERVERS
    log_line "[준비] 외부 API DNS: $AC2_DNS_SERVERS"
    if ! command -v mariadbd >/dev/null 2>&1; then
        stage='MariaDB 설치'
        log_line '[준비] Termux에 MariaDB 서버·클라이언트를 설치합니다.'
        run_logged pkg install -y mariadb
    fi
elif [ "$(uname -s)" = Darwin ]; then
    PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
    export PATH
    if ! command -v brew >/dev/null 2>&1; then
        stage='Homebrew 설치'
        log_line '[준비] Homebrew가 없어 먼저 설치합니다. macOS 명령줄 도구나 Homebrew 설치 중 관리자 비밀번호를 물을 수 있습니다.'
        command -v curl >/dev/null 2>&1 || { log_line 'Homebrew 준비에 curl이 필요합니다.'; exit 1; }
        repo_setup=$(mktemp "${TMPDIR:-/tmp}/ac2-homebrew.XXXXXX")
        run_logged curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh -o "$repo_setup"
        NONINTERACTIVE=1 run_logged /bin/bash "$repo_setup"
        rm -f -- "$repo_setup"
        repo_setup=
    fi
    if ! command -v mariadbd >/dev/null 2>&1; then
        stage='MariaDB 설치'
        log_line '[준비] Homebrew에서 MariaDB 최신 패키지를 설치합니다. 설치 중 관리자 비밀번호를 물을 수 있습니다.'
        run_logged brew install mariadb
    fi
else
    if ! command -v mariadbd >/dev/null 2>&1; then
        stage='MariaDB 설치'
        log_line '[준비] MariaDB 저장소와 서버·클라이언트 패키지를 준비합니다. sudo 사용 시 관리자 비밀번호를 물을 수 있습니다.'
        # The AC2 store requires 11.8+. Debian/Ubuntu's older default
        # packages do not meet that existing backend requirement.
        if command -v apt-get >/dev/null 2>&1 || command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1 || command -v zypper >/dev/null 2>&1; then
            command -v curl >/dev/null 2>&1 || { log_line 'MariaDB 저장소 준비에 curl이 필요합니다.'; exit 1; }
            run_logged mkdir -p bin/runtime
            repo_setup=$(mktemp "$package_dir/bin/runtime/mariadb-repo.XXXXXX")
            run_logged curl -fsSL https://r.mariadb.com/downloads/mariadb_repo_setup -o "$repo_setup"
            as_root bash "$repo_setup" --mariadb-server-version=mariadb-13.0 --skip-maxscale --skip-tools
            rm -f -- "$repo_setup"
            repo_setup=
        fi
        if command -v apt-get >/dev/null 2>&1; then
            as_root apt-get update
            as_root apt-get install -y mariadb-server mariadb-client
        elif command -v dnf >/dev/null 2>&1; then as_root dnf install -y MariaDB-server MariaDB-client
        elif command -v yum >/dev/null 2>&1; then as_root yum install -y MariaDB-server MariaDB-client
        elif command -v zypper >/dev/null 2>&1; then as_root zypper --non-interactive install MariaDB-server MariaDB-client
        elif command -v pacman >/dev/null 2>&1; then as_root pacman -S --needed --noconfirm mariadb
        elif command -v apk >/dev/null 2>&1; then as_root apk add --no-cache mariadb mariadb-client
        else log_line '지원되는 패키지 관리자가 없습니다. MariaDB 11.8 이상을 설치한 뒤 다시 실행해 주세요.'; exit 1
        fi
    fi
fi
stage='백엔드 실행 파일 확인'
log_line '[준비] 백엔드 실행 파일과 업데이트 도구를 확인합니다.'
run_logged chmod u+x ./bin/ac2
bootstrap="$package_dir/bin/ac2-updater"
state_file="$package_dir/.updates/update-state.json"
if [ ! -f "$state_file" ] && [ -f "$state_file.previous" ]; then state_file="$state_file.previous"; fi
if [ -f "$state_file" ]; then
    # Read only the shell bootstrap identity written by Go. The same path
    # containment as Go/AC1 keeps recovery inside the preserved runner area.
    state=$(sed -n 's/^[[:space:]]*"status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$state_file")
    case "$state" in
        applying|applied_pending_health)
            bound=$(sed -n 's/^[[:space:]]*"runner_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$state_file")
            if [ -n "$bound" ]; then
                runner_dir=$(CDPATH= cd -- "$(dirname -- "$package_dir/$bound")" && pwd -P)
                case "$runner_dir/" in
                    "$package_dir/.updates/runner/"*) bootstrap="$runner_dir/$(basename -- "$bound")" ;;
                    *) log_line 'runner_path escapes .updates/runner'; exit 1 ;;
                esac
            fi ;;
    esac
fi
stage='업데이트 도구 준비'
log_line '[준비] 업데이트 도구를 준비합니다.'
run_logged chmod u+x "$bootstrap"
output_dir=$(mktemp -d "${TMPDIR:-/tmp}/ac2-start-output.XXXXXX")
start_heartbeat
"$bootstrap" prepare-runner --root "$package_dir" <&3 > "$output_dir/runner" &
command_pid=$!
command_result=0
wait "$command_pid" || command_result=$?
command_pid=
runner=$(cat "$output_dir/runner")
stop_helpers
if [ "$command_result" -ne 0 ]; then
    if [ "$command_result" -eq 126 ] || [ "$command_result" -eq 127 ]; then
        log_line "업데이트 도구를 실행하지 못했습니다: $bootstrap (종료 코드: $command_result)"
    fi
    exit "$command_result"
fi
stage='백엔드 실행'
log_line '[준비] 백엔드를 실행합니다.'
"$runner" run --root "$package_dir" -- -managed "$@" <&3 &
command_pid=$!
command_result=0
wait "$command_pid" || command_result=$?
command_pid=
exit "$command_result"
