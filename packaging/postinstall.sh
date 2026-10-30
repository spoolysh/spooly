#!/bin/sh
set -e

if ! getent passwd spooly >/dev/null; then
	useradd --system --user-group --home-dir /var/lib/spooly --no-create-home --shell /usr/sbin/nologin spooly
fi

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload
	# Upgrades restart a running spooly; a new install waits for its config
	systemctl try-restart spooly.service
fi
