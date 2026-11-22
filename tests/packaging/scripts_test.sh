#!/bin/sh
# Table test of the package maintainer scripts in packaging/scripts (seam S1,
# D-102). Each case runs one script with the arguments its package manager
# passes, from a prepared state, and checks the exit status, the commands it
# ran and the files it left.
#
# The scripts set their own PATH, so the commands they call are replaced at
# their real paths by stubs that record each call: run this only as root in
# a throwaway container (task test-package-scripts). It refuses otherwise.
#
# POSIX sh and longer than a shell script should be: it runs where the
# scripts run (dash, busybox ash), with no other interpreter in the images.
#
# Usage: SENTINEL_DISPOSABLE_HOST=1 tests/packaging/scripts_test.sh <repo dir>
# Exit status: 0 all cases pass, 1 a case failed, 2 refused to run.
set -eu

[ "${SENTINEL_DISPOSABLE_HOST:-}" = 1 ] || {
	echo "scripts_test: replaces system commands; run it in a container (task test-package-scripts)" >&2
	exit 2
}
[ "$(id -u)" = 0 ] || {
	echo "scripts_test: must run as root" >&2
	exit 2
}
[ $# -eq 1 ] || {
	echo "usage: scripts_test.sh <repo dir>" >&2
	exit 2
}
readonly SCRIPTS="$1/packaging/scripts" WORK=/tmp/scripts-test
readonly STUBBED="systemctl deb-systemd-invoke rc-service rc-update groupadd addgroup getent"
failures=0
cases=0

# Where each stubbed command lives, so the scripts find it on their own PATH.
stub_path() {
	case $1 in
		systemctl | deb-systemd-invoke | getent) echo "/usr/bin/$1" ;;
		*) echo "/usr/sbin/$1" ;;
	esac
}

# reset removes everything a previous case created and installs every stub.
# A stub records "name args" in $WORK/calls, prints $WORK/stdout.<name>.<first
# arg> if there is one, and exits with the status in $WORK/rc.<name>.<first
# arg>, else $WORK/rc.<name>, else 0.
reset() {
	rm -rf "$WORK" /etc/sentinel /var/lib/sentinel /run/systemd/system /etc/runlevels /tmp/target
	mkdir -p "$WORK" /usr/sbin /usr/bin
	: >"$WORK/calls"
	cp "$WORK.group" /etc/group 2>/dev/null || cp /etc/group "$WORK.group"
	for name in $STUBBED; do
		write_stub "$(stub_path "$name")" "$name"
	done
	# After a stop, systemd reports the unit inactive unless a case says otherwise.
	prints systemctl show inactive
	chmod 0755 /etc /var /var/lib
	chown 0:0 /etc /var /var/lib
}

write_stub() {
	rm -f "$1"
	cat >"$1" <<-EOF
		#!/bin/sh
		echo "$2 \$*" >>$WORK/calls
		[ -f "$WORK/stdout.$2.\${1:-}" ] && cat "$WORK/stdout.$2.\${1:-}"
		[ -f "$WORK/rc.$2.\${1:-}" ] && exit "\$(cat "$WORK/rc.$2.\${1:-}")"
		[ -f $WORK/rc.$2 ] && exit "\$(cat $WORK/rc.$2)"
		exit 0
	EOF
	chmod 0755 "$1"
}

# Helpers for the arrange step of a case.
systemd_running() { mkdir -p /run/systemd/system; }
# prints <command> <first argument> <text>: what the stub prints for it
prints() { echo "$3" >"$WORK/stdout.$1.$2"; }
# fails <command> [status] [first argument]
fails() { echo "${2:-1}" >"$WORK/rc.$1${3:+.$3}"; }
absent() { rm -f "$(stub_path "$1")"; }
group_exists() { echo "sentinel:x:990:" >>/etc/group; }

# run <script> [args...]: runs packaging/scripts/<script>, keeps its status,
# stdout+stderr in $WORK/out.
run() {
	script=$SCRIPTS/$1
	shift
	set +e
	sh "$script" "$@" </dev/null >"$WORK/out" 2>&1
	status=$?
	set -e
}

fail() {
	echo "FAIL $case_name: $*" >&2
	sed 's/^/    | /' "$WORK/out" >&2
	case_failed=1
}

# Assertions.
want_status() { [ "$status" -eq "$1" ] || fail "exit status $status, want $1"; }
want_calls() {
	printf '%s\n' "$1" | sed '/^$/d' >"$WORK/want"
	cmp -s "$WORK/want" "$WORK/calls" || fail "calls:
$(cat "$WORK/calls")
want:
$(cat "$WORK/want")"
}
want_no_calls() { [ ! -s "$WORK/calls" ] || fail "unexpected calls: $(cat "$WORK/calls")"; }
want_output() { grep -q -- "$1" "$WORK/out" || fail "output does not contain \"$1\""; }
want_mode() {
	got=$(stat -c '%a %u:%g %F' "$1" 2>/dev/null || echo missing)
	[ "$got" = "$2" ] || fail "$1: $got, want $2"
}
want_file() { [ -f "$1" ] || fail "$1 missing"; }

# Each case is a function named case_<name>; this runs them all in order.
run_cases() {
	sed -n 's/^case_\([a-z0-9_]*\)().*/\1/p' "$0" >"$WORK.cases"
	while IFS= read -r case_name; do
		cases=$((cases + 1))
		case_failed=0
		reset
		"case_$case_name"
		if [ "$case_failed" -eq 0 ]; then
			echo "ok   $case_name"
		else
			failures=$((failures + 1))
		fi
	done <"$WORK.cases"
}

# --- preinstall.sh: every format, every pre-install and pre-upgrade event ---

case_pre_fresh_host_creates_group_and_state_dir() {
	run preinstall.sh install
	want_status 0
	want_calls "groupadd --system sentinel"
	want_mode /var/lib/sentinel "750 0:0 directory"
}

case_pre_existing_group_is_reused() {
	group_exists
	run preinstall.sh upgrade 0.3.0
	want_status 0
	want_no_calls
}

case_pre_alpine_uses_addgroup() {
	absent groupadd
	run preinstall.sh 0.3.1_pre5-r1
	want_status 0
	want_calls "addgroup -S sentinel"
}

case_pre_group_only_in_name_service_warns() {
	fails groupadd 9
	run preinstall.sh 1
	want_status 0
	want_calls "groupadd --system sentinel
getent group sentinel"
	want_output "numeric"
}

case_pre_group_creation_failure_stops() {
	fails groupadd 10
	fails getent 2
	run preinstall.sh 1
	want_status 1
	want_output "group sentinel"
}

case_pre_existing_state_dir_is_left_as_is() {
	mkdir -m 0700 /var/lib/sentinel
	echo audit >/var/lib/sentinel/marker
	group_exists
	run preinstall.sh upgrade 0.3.0
	want_status 0
	want_mode /var/lib/sentinel "700 0:0 directory"
	want_file /var/lib/sentinel/marker
}

case_pre_refuses_symlinked_state_dir() {
	mkdir -m 0777 /tmp/target
	ln -s /tmp/target /var/lib/sentinel
	run preinstall.sh install
	want_status 1
	want_output "/var/lib/sentinel is a symbolic link"
	want_no_calls
	want_mode /tmp/target "777 0:0 directory"
}

case_pre_refuses_symlink_to_a_safe_directory() {
	mkdir -m 0750 /tmp/target
	ln -s /tmp/target /etc/sentinel
	run preinstall.sh install
	want_status 1
	want_output "/etc/sentinel is a symbolic link"
	want_no_calls
}

case_pre_refuses_a_dangling_symlink() {
	ln -s /nonexistent /etc/sentinel
	run preinstall.sh install
	want_status 1
	want_output "/etc/sentinel is a symbolic link"
	want_no_calls
	[ ! -e /var/lib/sentinel ] || fail "state dir created"
}

case_pre_refuses_group_writable_config_dir() {
	mkdir -m 0770 /etc/sentinel
	chmod 0770 /etc/sentinel
	run preinstall.sh install
	want_status 1
	want_output "/etc/sentinel"
	want_no_calls
}

case_pre_refuses_world_writable_state_dir() {
	mkdir /var/lib/sentinel
	chmod 0757 /var/lib/sentinel
	run preinstall.sh 1
	want_status 1
	want_output "/var/lib/sentinel"
}

case_pre_refuses_state_dir_not_owned_by_root() {
	mkdir -m 0750 /var/lib/sentinel
	chown 1000:1000 /var/lib/sentinel
	run preinstall.sh 1
	want_status 1
	want_output "/var/lib/sentinel"
}

case_pre_refuses_state_path_that_is_a_file() {
	echo x >/var/lib/sentinel
	run preinstall.sh 1
	want_status 1
	want_output "/var/lib/sentinel"
}

case_pre_refuses_a_writable_parent() {
	chmod 0777 /var/lib
	run preinstall.sh install
	want_status 1
	want_output "/var/lib is writable by group or others"
	want_no_calls
	[ ! -e /var/lib/sentinel ] || fail "state dir created under an unsafe parent"
}

case_pre_refuses_a_parent_not_owned_by_root() {
	chown 1000 /etc
	run preinstall.sh install
	want_status 1
	want_output "/etc is not owned by root"
}

case_pre_deb_abort_upgrade_does_nothing() {
	mkdir -m 0777 /etc/sentinel
	chmod 0777 /etc/sentinel
	run preinstall.sh abort-upgrade 0.3.1
	want_status 0
	want_no_calls
}

case_pre_group_warning_still_creates_state_dir() {
	fails groupadd 9
	run preinstall.sh install
	want_status 0
	want_mode /var/lib/sentinel "750 0:0 directory"
}

# --- deb: postinst, prerm, postrm (Debian Policy, chapter 6) ---

case_deb_postinst_install_reloads_only() {
	systemd_running
	run deb/postinst configure ""
	want_status 0
	want_calls "systemctl daemon-reload"
}

case_deb_postinst_upgrade_restarts_if_running() {
	systemd_running
	run deb/postinst configure 0.3.0
	want_status 0
	want_calls "systemctl daemon-reload
deb-systemd-invoke try-restart sentineld.service"
}

case_deb_postinst_first_configure_without_version() {
	systemd_running
	run deb/postinst configure
	want_status 0
	want_calls "systemctl daemon-reload"
}

case_deb_postinst_upgrade_without_invoke_helper() {
	systemd_running
	absent deb-systemd-invoke
	run deb/postinst configure 0.3.0
	want_status 0
	want_calls "systemctl daemon-reload
systemctl try-restart sentineld.service"
}

case_deb_postinst_failed_restart_is_reported() {
	systemd_running
	fails deb-systemd-invoke
	run deb/postinst configure 0.3.0
	want_status 0
	want_output "systemctl status sentineld"
}

case_deb_postinst_without_systemd_does_nothing() {
	run deb/postinst configure 0.3.0
	want_status 0
	want_no_calls
}

case_deb_postinst_abort_events_do_nothing() {
	systemd_running
	for event in abort-upgrade abort-remove abort-deconfigure; do
		run deb/postinst "$event" 0.3.1
		want_status 0
	done
	want_no_calls
}

case_deb_postinst_unknown_event_fails() {
	run deb/postinst bogus
	want_status 1
}

case_deb_prerm_remove_stops_and_disables() {
	systemd_running
	run deb/prerm remove
	want_status 0
	want_calls "deb-systemd-invoke stop sentineld.service
systemctl show -p ActiveState --value sentineld.service
systemctl disable sentineld.service"
}

case_deb_prerm_stop_denied_by_policy_fails_removal() {
	systemd_running
	# deb-systemd-invoke exits 0 when policy-rc.d denies the stop.
	prints systemctl show active
	run deb/prerm remove
	want_status 1
	want_output "still active"
}

case_deb_prerm_unit_still_stopping_fails_removal() {
	systemd_running
	prints systemctl show deactivating
	run deb/prerm remove
	want_status 1
	want_output "still deactivating"
}

case_deb_prerm_unknown_state_fails_removal() {
	systemd_running
	fails systemctl 1 show
	run deb/prerm remove
	want_status 1
}

case_deb_prerm_unit_not_loaded_still_disables() {
	systemd_running
	fails deb-systemd-invoke 5 stop
	run deb/prerm remove
	want_status 0
	want_calls "deb-systemd-invoke stop sentineld.service
systemctl show -p ActiveState --value sentineld.service
systemctl disable sentineld.service"
}

case_deb_prerm_failed_stop_fails_removal() {
	systemd_running
	fails deb-systemd-invoke
	run deb/prerm remove
	want_status 1
	want_calls "deb-systemd-invoke stop sentineld.service"
}

case_deb_prerm_failed_disable_fails_removal() {
	systemd_running
	fails systemctl 1 disable
	run deb/prerm remove
	want_status 1
}

case_deb_prerm_remove_offline_only_disables() {
	run deb/prerm remove
	want_status 0
	want_calls "systemctl disable sentineld.service"
}

case_deb_prerm_upgrade_events_do_nothing() {
	systemd_running
	for event in upgrade deconfigure failed-upgrade; do
		run deb/prerm "$event" 0.3.1
		want_status 0
	done
	want_no_calls
}

case_deb_prerm_unknown_event_fails() {
	run deb/prerm bogus
	want_status 1
}

case_deb_postrm_keeps_state() {
	systemd_running
	mkdir -m 0750 /var/lib/sentinel
	echo audit >/var/lib/sentinel/marker
	for event in remove purge upgrade failed-upgrade abort-install abort-upgrade disappear; do
		run deb/postrm "$event"
		want_status 0
	done
	want_calls "systemctl daemon-reload
systemctl daemon-reload"
	want_file /var/lib/sentinel/marker
}

case_deb_postrm_unknown_event_fails() {
	run deb/postrm bogus
	want_status 1
}

# --- rpm: %post, %preun, %postun; $1 counts the installed instances ---

case_rpm_post_install_reloads_only() {
	systemd_running
	run rpm/post 1
	want_status 0
	want_calls "systemctl daemon-reload"
}

case_rpm_post_upgrade_restarts_if_running() {
	systemd_running
	for count in 2 3; do
		run rpm/post "$count"
		want_status 0
	done
	want_calls "systemctl daemon-reload
systemctl try-restart sentineld.service
systemctl daemon-reload
systemctl try-restart sentineld.service"
}

case_rpm_post_failed_restart_is_reported() {
	systemd_running
	fails systemctl 1 try-restart
	run rpm/post 2
	want_status 0
	want_output "systemctl status sentineld"
}

case_rpm_post_without_systemd_does_nothing() {
	run rpm/post 2
	want_status 0
	want_no_calls
}

case_rpm_preun_erase_stops_and_disables() {
	systemd_running
	run rpm/preun 0
	want_status 0
	want_calls "systemctl stop sentineld.service
systemctl show -p ActiveState --value sentineld.service
systemctl disable sentineld.service"
}

case_rpm_preun_still_running_fails_erase() {
	systemd_running
	prints systemctl show active
	run rpm/preun 0
	want_status 1
	want_output "still active"
}

case_rpm_preun_unknown_state_fails_erase() {
	systemd_running
	fails systemctl 1 show
	run rpm/preun 0
	want_status 1
}

case_rpm_preun_unit_not_loaded_still_disables() {
	systemd_running
	fails systemctl 5 stop
	run rpm/preun 0
	want_status 0
	want_calls "systemctl stop sentineld.service
systemctl show -p ActiveState --value sentineld.service
systemctl disable sentineld.service"
}

case_rpm_preun_failed_stop_fails_erase() {
	systemd_running
	fails systemctl
	run rpm/preun 0
	want_status 1
	want_calls "systemctl stop sentineld.service"
}

case_rpm_preun_upgrade_does_nothing() {
	systemd_running
	run rpm/preun 1
	want_status 0
	want_no_calls
}

case_rpm_postun_erase_reloads_and_keeps_state() {
	systemd_running
	mkdir -m 0750 /var/lib/sentinel
	echo audit >/var/lib/sentinel/marker
	run rpm/postun 0
	want_status 0
	want_calls "systemctl daemon-reload"
	want_file /var/lib/sentinel/marker
}

case_rpm_postun_upgrade_does_nothing() {
	systemd_running
	run rpm/postun 1
	want_status 0
	want_no_calls
}

case_rpm_scripts_refuse_a_non_count() {
	for script in rpm/post rpm/preun rpm/postun; do
		run "$script" upgrade
		want_status 1
	done
}

# --- apk: post-upgrade and pre-deinstall (OpenRC); arguments are versions ---

in_runlevel() {
	mkdir -p "/etc/runlevels/$1"
	ln -s /etc/init.d/sentineld "/etc/runlevels/$1/sentineld"
}

case_apk_post_upgrade_restarts_if_started() {
	run apk/post-upgrade 0.3.1-r1 0.3.0-r1
	want_status 0
	want_calls "rc-service --ifstarted sentineld restart"
}

case_apk_post_upgrade_failed_restart_is_reported() {
	fails rc-service
	run apk/post-upgrade 0.3.1-r1 0.3.0-r1
	want_status 0
	want_output "rc-service sentineld status"
}

case_apk_post_upgrade_without_openrc_does_nothing() {
	absent rc-service
	run apk/post-upgrade 0.3.1-r1 0.3.0-r1
	want_status 0
	want_no_calls
}

case_apk_pre_deinstall_stops_and_removes_from_runlevels() {
	in_runlevel boot
	in_runlevel default
	run apk/pre-deinstall 0.3.1-r1
	want_status 0
	want_calls "rc-service --ifstarted sentineld stop
rc-update del sentineld boot
rc-update del sentineld default"
}

case_apk_pre_deinstall_not_in_a_runlevel_only_stops() {
	run apk/pre-deinstall 0.3.1-r1
	want_status 0
	want_calls "rc-service --ifstarted sentineld stop"
}

case_apk_pre_deinstall_failed_stop_fails_removal() {
	in_runlevel default
	fails rc-service
	run apk/pre-deinstall 0.3.1-r1
	want_status 1
	want_calls "rc-service --ifstarted sentineld stop"
}

case_apk_pre_deinstall_keeps_state() {
	mkdir -m 0750 /var/lib/sentinel
	echo audit >/var/lib/sentinel/marker
	run apk/pre-deinstall 0.3.1-r1
	want_status 0
	want_file /var/lib/sentinel/marker
}

run_cases
echo "$cases cases, $failures failed"
[ "$failures" -eq 0 ]
