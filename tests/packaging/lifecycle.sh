#!/bin/sh
# Package lifecycle on a real host (seam S3, D-102): install, upgrade, remove
# and purge the sentinel-watchdog package with the host's own package manager
# and init system, and check what the package promises. POSIX sh, because
# Alpine has no bash; longer than a shell script should be, but the hosts
# it runs on have no other interpreter in common.
#
# It installs and removes packages, creates users and changes /etc: run it
# only as root on a disposable VM (task test-package). It refuses otherwise.
#
# Usage: SENTINEL_DISPOSABLE_HOST=1 lifecycle.sh <base package> <next package>
#   <next> must be a newer version of <base>, and ship a different
#   /etc/sentinel/sentinel.yaml (task package-pair builds both).
# Exit status: 0 every check passed, 1 a check failed, 2 refused or usage.
set -eu
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH

[ "${SENTINEL_DISPOSABLE_HOST:-}" = 1 ] || {
	echo "lifecycle: changes the host; run it on a disposable VM (task test-package)" >&2
	exit 2
}
[ "$(id -u)" = 0 ] || {
	echo "lifecycle: must run as root" >&2
	exit 2
}
[ $# -eq 2 ] && [ -f "$1" ] && [ -f "$2" ] || {
	echo "usage: lifecycle.sh <base package> <next package>" >&2
	exit 2
}
BASE=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
NEXT=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
readonly BASE NEXT CONFIG=/etc/sentinel/sentinel.yaml STATE=/var/lib/sentinel
readonly SOCKET=/run/sentinel/sentinel.sock MARK="# lifecycle: local edit"
# The process service of the local edit, and how to find it.
readonly WORKER_ARG=31337 WORKER_MATCH='^/bin/sleep 31337'
readonly GROUP_GID=4242
failures=0
step=setup

# A host that has Sentinel's state is not disposable: refuse before any change.
if [ -e /var/lib/sentinel ] || [ -L /var/lib/sentinel ] || [ -e /etc/sentinel ] || [ -L /etc/sentinel ]; then
	echo "lifecycle: /etc/sentinel or /var/lib/sentinel exists: not a fresh disposable host" >&2
	exit 2
fi
TMP=$(mktemp -d)
readonly TMP
trap 'rm -rf "$TMP"' EXIT

say() { echo "---- $*"; }
fail() {
	echo "FAIL [$step] $*" >&2
	failures=$((failures + 1))
}
check() { # check <description> <command...>
	desc=$1
	shift
	if "$@" >/dev/null 2>&1; then echo "ok   [$step] $desc"; else fail "$desc"; fi
}
refute() { # refute <description> <command...>
	desc=$1
	shift
	if "$@" >/dev/null 2>&1; then fail "$desc"; else echo "ok   [$step] $desc"; fi
}

# wait_for <seconds> <command...>: polls until the command succeeds.
wait_for() {
	limit=$1
	shift
	i=0
	while [ "$i" -lt "$((limit * 4))" ]; do
		"$@" >/dev/null 2>&1 && return 0
		sleep 0.25
		i=$((i + 1))
	done
	return 1
}

# --- the host's package manager -------------------------------------------

case $BASE in
	*.deb) format=deb ;;
	*.rpm) format=rpm ;;
	*.apk) format=apk ;;
	*)
		echo "lifecycle: unknown package type: $BASE" >&2
		exit 2
		;;
esac
if [ "$format" = rpm ] && command -v zypper >/dev/null 2>&1; then
	format=zypper
fi

# The commands docs/operations.md documents, made non-interactive. deb keeps the
# administrator's configuration on upgrade (confold), as an answer would.
# No -q: dnf -q hides what the package scripts print.
install_pkg() {
	case $format in
		deb) DEBIAN_FRONTEND=noninteractive apt-get install -y -o Dpkg::Options::=--force-confold "$1" ;;
		rpm) dnf install -y "$1" ;;
		zypper) zypper -n install --allow-unsigned-rpm "$1" ;;
		apk) apk add --allow-untrusted "$1" ;;
	esac
}
remove_pkg() {
	case $format in
		deb) DEBIAN_FRONTEND=noninteractive apt-get remove -y sentinel-watchdog ;;
		rpm) dnf remove -y sentinel-watchdog ;;
		zypper) zypper -n remove sentinel-watchdog ;;
		apk) apk del sentinel-watchdog ;;
	esac
}
purge_pkg() {
	case $format in
		deb) DEBIAN_FRONTEND=noninteractive apt-get purge -y sentinel-watchdog ;;
		apk) apk del --purge sentinel-watchdog ;;
		*) return 0 ;; # rpm has no purge
	esac
}
installed_version() {
	case $format in
		deb) dpkg-query -W -f '${Version}' sentinel-watchdog 2>/dev/null ;;
		rpm | zypper) rpm -q --qf '%{VERSION}-%{RELEASE}' sentinel-watchdog 2>/dev/null ;;
		apk) apk info -e -v sentinel-watchdog 2>/dev/null | sed 's/^sentinel-watchdog-//' ;;
	esac
}
pkg_version() { # the version a package file carries
	case $format in
		deb) dpkg-deb -f "$1" Version ;;
		rpm | zypper) rpm -qp --nosignature --qf '%{VERSION}-%{RELEASE}' "$1" ;;
		apk) tar -xzOf "$1" .PKGINFO 2>/dev/null | sed -n 's/^pkgver = //p' ;;
	esac
}
is_installed() { # installed, not only "removed, configuration left"
	case $format in
		deb) dpkg-query -W -f '${Status}' sentinel-watchdog 2>/dev/null | grep -q 'install ok installed' ;;
		rpm | zypper) rpm -q sentinel-watchdog >/dev/null 2>&1 ;;
		apk) apk info -e sentinel-watchdog >/dev/null 2>&1 ;;
	esac
}
older() { # older <a> <b>: a sorts before b for this package manager
	case $format in
		deb) dpkg --compare-versions "$1" lt "$2" ;;
		rpm | zypper) [ "$(rpm --eval "%{lua: print(rpm.vercmp('$1', '$2'))}")" = -1 ] ;;
		apk) [ "$(apk version -t "$1" "$2")" = "<" ] ;;
	esac
}
# The file a package manager leaves for a new shipped configuration on
# upgrade, and for a modified one on removal (Q-D1: native behaviour).
case $format in
	deb) new_default=$CONFIG.dpkg-dist ;;
	rpm | zypper) new_default=$CONFIG.rpmnew ;;
	apk) new_default=$CONFIG.apk-new ;;
esac

# --- the host's init system -----------------------------------------------

if [ -d /run/systemd/system ]; then
	init=systemd
else
	init=openrc
fi
svc() { # svc start|stop|restart|reload
	sh -c "$(svc_cmd "$1")"
}
svc_cmd() { # the init system's command line for an action
	case $init in
		systemd) echo "systemctl $1 sentineld.service" ;;
		openrc) echo "rc-service sentineld $1" ;;
	esac
}
enable_svc() {
	case $init in
		systemd) systemctl enable sentineld.service ;;
		openrc) rc-update add sentineld default ;;
	esac
}
is_active() {
	case $init in
		systemd) systemctl is-active -q sentineld.service ;;
		openrc) rc-service sentineld status ;;
	esac
}
is_enabled() {
	case $init in
		systemd) systemctl is-enabled -q sentineld.service ;;
		openrc) [ -L /etc/runlevels/default/sentineld ] ;;
	esac
}
started() { # active and answering on its socket
	wait_for 20 is_active && wait_for 20 test -S "$SOCKET"
}
# start_ready: the init system's start returns within 30 s, and only once
# sentineld answers (Type=notify; OpenRC's readiness, openrc-contract.md).
start_ready() {
	timeout 30 sh -c "$(svc_cmd start)" && sentinelctl status
}
config_digest() { sentinelctl status | sed -n 's/^configuration: //p'; }
# By command line: busybox pgrep -x does not match it under supervise-daemon.
daemon_pid() { pgrep -f '^/usr/bin/sentineld ' || true; }
worker_pid() { pgrep -f "$WORKER_MATCH" || true; }
# An enable link of either init system left behind.
enable_link_left() {
	for link in /etc/systemd/system/multi-user.target.wants/sentineld.service /etc/runlevels/*/sentineld; do
		[ -L "$link" ] && return 0
	done
	return 1
}

# --- fixtures ---------------------------------------------------------------

add_user() { # add_user <name>
	id "$1" >/dev/null 2>&1 && return 0
	if command -v useradd >/dev/null 2>&1; then
		useradd -M -s /bin/sh "$1"
	else
		adduser -D -H -s /bin/sh "$1"
	fi
}
join_group() { # join_group <user>: adds the user to group sentinel
	if command -v usermod >/dev/null 2>&1; then
		usermod -a -G sentinel "$1"
	else
		addgroup "$1" sentinel
	fi
}
remove_group() {
	if command -v groupdel >/dev/null 2>&1; then
		groupdel sentinel
	else
		delgroup sentinel
	fi
}
make_group() { # the group sentinel with a fixed gid
	if command -v groupadd >/dev/null 2>&1; then
		groupadd -g "$GROUP_GID" sentinel
	else
		addgroup -g "$GROUP_GID" sentinel
	fi
}
as_user() { # as_user <name> <command>
	su -s /bin/sh "$1" -c "$2"
}
# A local edit: the supervisor with one process service, so an upgrade
# restart shows that sentineld stops and starts what it supervises.
edit_config() {
	cat >>"$CONFIG" <<-EOF
		$MARK
		modules:
		  supervisor:
		    enabled: true
		    processes: true
	EOF
	mkdir -p /etc/sentinel/supervisor
	cat >/etc/sentinel/supervisor/lifecycle.yaml <<-EOF
		version: 1
		services:
		  - name: lifecycle-worker
		    command: /bin/sleep
		    args: ["$WORKER_ARG"]
	EOF
}
# clean_host undoes what a sequence left: the package, its files, the state
# and the group (each sequence starts from a host that never had Sentinel).
clean_host() {
	{
		! is_installed || svc stop || true
		remove_pkg || true
		purge_pkg || true
		grep -q '^sentinel:' /etc/group && remove_group || true
	} >/dev/null 2>&1
	rm -rf /etc/sentinel "$STATE" "$TMP/target"
	for f in /etc/systemd/system/multi-user.target.wants/sentineld.service /etc/runlevels/*/sentineld; do
		[ ! -L "$f" ] || rm -f "$f"
	done
}

# --- sequences --------------------------------------------------------------

sequence_versions() {
	step=versions
	check "base $(pkg_version "$BASE") sorts before next $(pkg_version "$NEXT")" older "$(pkg_version "$BASE")" "$(pkg_version "$NEXT")"
}

# A: install, nothing started; an upgrade of a stopped service keeps it stopped.
sequence_stopped() {
	clean_host
	step="A install"
	check "install base" install_pkg "$BASE"
	check "installed version is base" test "$(installed_version)" = "$(pkg_version "$BASE")"
	refute "not started on install" is_active
	refute "not enabled on install" is_enabled
	check "config 0640 root" test "$(stat -c '%a %u' "$CONFIG")" = "640 0"
	check "config dir 0750 root" test "$(stat -c '%a %u' /etc/sentinel)" = "750 0"
	check "state dir 0750 root" test "$(stat -c '%a %u' "$STATE")" = "750 0"
	check "group sentinel created" grep -q '^sentinel:' /etc/group
	check "installed config is valid" sentineld -validate -config "$CONFIG"
	[ "$init" = openrc ] || check "unit verifies" systemd-analyze verify sentineld.service
	step="A upgrade stopped"
	check "upgrade to next" install_pkg "$NEXT"
	check "installed version is next" test "$(installed_version)" = "$(pkg_version "$NEXT")"
	refute "still not started" is_active
	refute "still not enabled" is_enabled
	step="A remove"
	check "remove" remove_pkg
	check "state dir kept" test -d "$STATE"
}

# B: enabled and running; reload and upgrade; the administrator's
# configuration survives; removal stops and disables it.
sequence_running() {
	clean_host
	step="B install"
	check "install base" install_pkg "$BASE"
	edit_config
	echo audit >"$STATE/lifecycle-marker"
	check "enable" enable_svc
	check "start returns once sentineld answers" start_ready
	check "started" started
	check "worker started" wait_for 20 pgrep -f "$WORKER_MATCH"
	pid=$(daemon_pid)
	check "sentineld has a pid" test -n "$pid"
	digest=$(config_digest)
	echo "# lifecycle: a change for reload" >>"$CONFIG"
	check "reload through the init system" svc reload
	check "reload keeps the process (pid $pid)" test "$(daemon_pid)" = "$pid"
	check "reload applied the change" sh -c "[ -n '$digest' ] && [ \"\$(sentinelctl status | sed -n 's/^configuration: //p')\" != '$digest' ]"
	digest=$(config_digest)
	echo "no_such_key: true" >>"$CONFIG"
	refute "reload refuses a rejected configuration" svc reload
	check "and the daemon keeps running" test "$(daemon_pid)" = "$pid"
	check "with the last good configuration" test "$(config_digest)" = "$digest"
	sed -i '/^no_such_key/d' "$CONFIG"
	add_user lifecycle-member
	add_user lifecycle-other
	check "users exist" id lifecycle-other
	join_group lifecycle-member
	check "member reads status" as_user lifecycle-member "sentinelctl status"
	as_user lifecycle-member "sentinelctl reload" >"$TMP/out" 2>&1 && fail "member could reload"
	check "member refused reload (operate tier)" grep -q "needs the operate tier" "$TMP/out"
	as_user lifecycle-other "sentinelctl status" >"$TMP/out" 2>&1 && fail "other user read the status"
	check "other user refused at the socket" grep -q "no access to $SOCKET" "$TMP/out"
	worker=$(worker_pid)
	check "the worker has a pid" test -n "$worker"
	step="B upgrade running"
	check "upgrade to next" install_pkg "$NEXT"
	check "started again" started
	check "sentineld restarted (pid $pid)" test "$(daemon_pid)" != "$pid"
	check "worker restarted (pid $worker)" wait_for 20 sh -c "p=\$(pgrep -f '$WORKER_MATCH'); [ -n \"\$p\" ] && [ \"\$p\" != '$worker' ]"
	check "still enabled" is_enabled
	check "local edit kept" grep -qF "$MARK" "$CONFIG"
	check "new default beside it ($new_default)" test -f "$new_default"
	check "state kept" test -f "$STATE/lifecycle-marker"
	step="B remove running"
	check "remove while running and enabled" remove_pkg
	refute "process gone" test -n "$(daemon_pid)"
	refute "worker gone" test -n "$(worker_pid)"
	refute "socket gone" test -S "$SOCKET"
	refute "no enable link left" enable_link_left
	check "state kept" test -f "$STATE/lifecycle-marker"
	check "group kept" grep -q '^sentinel:' /etc/group
	case $format in
		deb | apk) check "modified config kept on remove" grep -qF "$MARK" "$CONFIG" ;;
		rpm | zypper) check "modified config saved as .rpmsave" grep -qF "$MARK" "$CONFIG.rpmsave" ;;
	esac
}

# C: a configuration the new version rejects: the package upgrade still
# succeeds and says so; the service is down until the configuration is fixed
# (Q-D6b).
sequence_bad_config() {
	clean_host
	step="C bad config"
	check "install base" install_pkg "$BASE"
	check "start" svc start
	check "started" started
	echo "no_such_key: true" >>"$CONFIG"
	install_pkg "$NEXT" >"$TMP/out" 2>&1 || fail "upgrade with a bad config failed the package"
	check "the upgrade says sentineld did not restart" grep -q "did not restart" "$TMP/out"
	refute "service not running a rejected config" is_active
	# systemd stops retrying after a few failed starts (start limit) until
	# told otherwise: the step docs/operations.md gives the administrator.
	[ "$init" = openrc ] || systemctl reset-failed sentineld.service
	# Refused by the script itself (exit 1 within its 20 s), not by this
	# test's timeout (124).
	status=0
	timeout 40 sh -c "$(svc_cmd start)" >/dev/null 2>&1 || status=$?
	check "start refuses a rejected configuration (exit $status)" sh -c "[ $status -ne 0 ] && [ $status -ne 124 ]"
	refute "and no supervisor or daemon is left" test -n "$(daemon_pid)"
	refute "and it is not left looking started" is_active
	sed -i '/^no_such_key/d' "$CONFIG"
	[ "$init" = openrc ] || systemctl reset-failed sentineld.service
	check "starts once fixed" svc restart
	check "started" started
	check "remove" remove_pkg
}

# D: unsafe paths stop the install (apk installs anyway and sentineld refuses
# them at start); an existing group is reused, members and gid unchanged.
sequence_preexisting() {
	clean_host
	step="D symlink"
	# The group exists, so on apk the only reason sentineld can refuse is the path.
	make_group
	mkdir -m 0750 "$TMP/target"
	ln -s "$TMP/target" "$STATE"
	# sentineld itself accepts a symlink to a safe directory (its own trust
	# rules); the package is stricter.
	unsafe_install "/var/lib/sentinel is a symbolic link" accepted
	rm -f "$STATE"
	clean_host
	step="D writable"
	make_group
	mkdir -m 0777 /etc/sentinel
	chmod 0777 /etc/sentinel
	unsafe_install "/etc/sentinel is writable by group or others" refused
	check "the unsafe directory is left as it was" test "$(stat -c %a /etc/sentinel)" = 777
	clean_host
	step="D group"
	make_group
	add_user lifecycle-member
	join_group lifecycle-member
	check "install with the group already there" install_pkg "$BASE"
	check "one sentinel group" test "$(grep -c '^sentinel:' /etc/group)" = 1
	check "gid unchanged" test "$(grep '^sentinel:' /etc/group | cut -d: -f3)" = "$GROUP_GID"
	check "member kept" sh -c "grep '^sentinel:' /etc/group | cut -d: -f4 | grep -q lifecycle-member"
	check "remove" remove_pkg
}

# unsafe_install <message> <refused|accepted>: the install fails with the
# script's message; deb and rpm leave the package out, apk installs it anyway
# (apk-tools cannot veto from a script), and sentineld then refuses to start
# on that path when its own checks refuse it.
unsafe_install() {
	install_pkg "$BASE" >"$TMP/out" 2>&1 && fail "install over an unsafe path succeeded"
	check "the install names the problem: $1" grep -qF "$1" "$TMP/out"
	if [ "$format" = apk ]; then
		check "apk installed it anyway (known apk-tools limit)" is_installed
		[ "$2" = refused ] || return 0
		# Exit 1 at start, the path named; a run until the timeout ends with
		# SIGTERM and exit 0 (busybox timeout) or 124.
		status=0
		timeout 10 sentineld -config "$CONFIG" >"$TMP/run" 2>&1 || status=$?
		check "sentineld refuses to start on it (exit $status)" test "$status" = 1
		check "and names the path" grep -qF "${1%% *}" "$TMP/run"
	else
		refute "package not installed" is_installed
	fi
}

# E: purge removes what the package manager owns, never the state, the audit
# log or the group (Q-D1).
sequence_purge() {
	[ "$format" = deb ] || [ "$format" = apk ] || return 0
	clean_host
	step="E purge"
	check "install base" install_pkg "$BASE"
	echo "$MARK" >>"$CONFIG"
	echo audit >"$STATE/lifecycle-marker"
	check "purge" purge_pkg
	refute "modified config deleted by purge" test -e "$CONFIG"
	check "state kept" test -f "$STATE/lifecycle-marker"
	check "group kept" grep -q '^sentinel:' /etc/group
}

# F (OpenRC): a non-default daemon.socket, named in /etc/conf.d/sentineld,
# is what readiness and reload use.
sequence_custom_socket() {
	[ "$init" = openrc ] || return 0
	clean_host
	step="F custom socket"
	check "install base" install_pkg "$BASE"
	printf 'daemon:\n  socket: /run/sentinel/other.sock\n  socket_group: sentinel\n' >"$TMP/daemon"
	sed -i '/^daemon:/,$d' "$CONFIG"
	cat "$TMP/daemon" >>"$CONFIG"
	echo 'sentinel_socket=/run/sentinel/other.sock' >/etc/conf.d/sentineld
	check "start with the socket from conf.d" timeout 40 rc-service sentineld start
	check "answers on that socket" sentinelctl status -socket /run/sentinel/other.sock
	check "reload through it" rc-service sentineld reload
	check "stop" rc-service sentineld stop
	rm -f /etc/conf.d/sentineld
	check "remove" remove_pkg
}

say "$format packages, $init: $(basename "$BASE") -> $(basename "$NEXT")"
sequence_versions
sequence_stopped
sequence_running
sequence_bad_config
sequence_preexisting
sequence_purge
sequence_custom_socket
clean_host
rm -f "$STATE" 2>/dev/null || true
if [ "$failures" -gt 0 ]; then
	say "lifecycle: $failures checks failed"
	exit 1
fi
say "lifecycle: ok"
