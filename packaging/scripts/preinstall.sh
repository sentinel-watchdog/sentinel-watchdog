#!/bin/sh
# sentinel-watchdog: before install and upgrade, in every package format
# (deb preinst, rpm %pre, apk pre-install and pre-upgrade; D-102).
#
# The same steps are right for every install and upgrade, so the arguments
# are ignored, except deb's "abort-upgrade" (a rollback: nothing to do):
#   1. refuse to install over an unsafe /etc/sentinel or /var/lib/sentinel:
#      a symlink, not a directory, not owned by root, or writable by group or
#      others (a non-root user could have placed files there), or under a
#      parent (/, /etc, /var, /var/lib) that is not root's alone;
#   2. create the system group "sentinel" (socket read tier) unless
#      /etc/group has it: sentineld is a static binary and resolves group
#      names from /etc/group only;
#   3. create /var/lib/sentinel (state and audit log). It is not a package
#      file, so no package manager ever removes it.
#
# apk records a failure here but installs the package anyway (apk-tools
# 2.14): sentineld's own checks then apply at start.
set -eu
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH

refuse() {
	echo "sentinel-watchdog: $1 $2; fix or remove it as root, then install again" >&2
	exit 1
}

# check_owner_mode refuses a directory not owned by root or writable by
# group or others.
check_owner_mode() {
	[ "$(stat -c %u "$1")" = 0 ] || refuse "$1" "is not owned by root"
	# The last two digits of the octal mode: group and others.
	perms=$(stat -c %a "$1")
	group=$(echo "$perms" | sed 's/.*\(.\).$/\1/')
	other=$(echo "$perms" | sed 's/.*\(.\)$/\1/')
	case "$group$other" in
		*[2367]*) refuse "$1" "is writable by group or others (mode $perms)" ;;
	esac
}

# check_dir refuses an existing path that a non-root user could control.
check_dir() {
	[ -L "$1" ] && refuse "$1" "is a symbolic link"
	[ -e "$1" ] || return 0
	[ -d "$1" ] || refuse "$1" "is not a directory"
	check_owner_mode "$1"
}

ensure_group() {
	grep -q '^sentinel:' /etc/group && return 0
	if command -v groupadd >/dev/null 2>&1; then
		groupadd --system sentinel && return 0
	else
		addgroup -S sentinel && return 0
	fi
	if getent group sentinel >/dev/null 2>&1; then
		echo "sentinel-watchdog: group sentinel exists only in the name service, not in /etc/group;" \
			"sentineld reads /etc/group only: set daemon.socket_group in /etc/sentinel/sentinel.yaml" \
			"to the group's numeric id" >&2
		return 0
	fi
	echo "sentinel-watchdog: cannot create the system group sentinel" >&2
	exit 1
}

[ "${1:-}" != abort-upgrade ] || exit 0

for parent in / /etc /var /var/lib; do
	check_dir "$parent"
done
check_dir /etc/sentinel
check_dir /var/lib/sentinel
ensure_group
[ -e /var/lib/sentinel ] || mkdir -m 0750 /var/lib/sentinel
