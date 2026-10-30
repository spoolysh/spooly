#!/bin/sh
set -e

# deb passes "remove", rpm passes 0; both pass something else on upgrade
case "$1" in
remove | 0)
	if [ -d /run/systemd/system ]; then
		systemctl disable --now boxy.service || true
	fi
	;;
esac
