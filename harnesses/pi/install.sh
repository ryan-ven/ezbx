#!/bin/sh
# Installs the Pi coding agent into the box image.
# Runs once at image build time, as root, before the unprivileged user is set up.
# Usage: install.sh <version>
set -eu

VERSION="${1:?Pi version required, for example 1.5.0}"

npm install -g "@earendil-works/pi-coding-agent@${VERSION}"
npm cache clean --force

# Fail the build now if the command is missing, not later at run time.
command -v pi >/dev/null 2>&1
