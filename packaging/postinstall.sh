#!/bin/sh
set -e

if ! getent passwd boxy >/dev/null; then
	useradd --system --user-group --home-dir /var/lib/boxy --no-create-home --shell /usr/sbin/nologin boxy
fi

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload
	# Upgrades restart a running boxy; a new install waits for its config
	systemctl try-restart boxy.service
fi
