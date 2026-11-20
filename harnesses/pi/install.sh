#!/bin/sh
# Installs the Pi coding agent for the box image.
#
# Runs at image build time as an unprivileged user, in a folder holding this
# script, package.json and package-lock.json. The lockfile pins Pi and
# everything it pulls in, with integrity hashes, so a rebuild gets the same
# files, and npm refuses any download that doesn't match its hash.
#
# Install scripts are turned off. Two packages declare one: @google/genai
# (an echo that does nothing) and protobufjs (a version warning). Pi needs
# neither, so no package code runs during the build.
set -eu

npm ci --ignore-scripts --omit=dev --no-audit --no-fund
npm cache clean --force

# Fail the build now if the command is missing, not later at run time.
test -x node_modules/.bin/pi
