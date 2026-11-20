# EZBox box image: Node, git and the Pi harness, for an unprivileged user.
#
# Build from the repository root:
#   docker build -f docker/box.Dockerfile -t ezbx-box:dev .
#
# Run it with every capability dropped and no-new-privileges, attached only
# to the box's inside network (see docs/isolation-test.md).

# Node 24 LTS on Debian 12, pinned by digest so a rebuild starts from the
# same base.
FROM node:24.22.3-bookworm-slim@sha256:dc26ae92c9fb15cf6b1d63e20f0944b2efe0f115b20e00be48fb23fceb061c54 AS base

# Harness install: runs as the base image's unprivileged "node" user, so no
# package code could run as root even if install scripts were on.
FROM base AS pi
RUN mkdir /build && chown node:node /build
USER node
WORKDIR /build/pi
COPY --chown=node:node harnesses/pi/package.json harnesses/pi/package-lock.json harnesses/pi/install.sh ./
RUN sh install.sh

FROM base
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git \
 && rm -rf /var/lib/apt/lists/*

# The agent user replaces the base image's "node" user at the same id. No
# sudo is installed, and setuid and setgid bits are removed from every file,
# so nothing in the image can raise privileges.
RUN userdel -r node \
 && groupadd -g 1000 agent \
 && useradd -m -u 1000 -g agent -s /bin/bash agent \
 && mkdir /workspace && chown agent:agent /workspace \
 && find / -xdev -perm /6000 -type f -exec chmod a-s {} +

# Harness files belong to root, so the agent can run Pi but not change it.
COPY --from=pi --chown=root:root /build/pi /opt/ezbx/harnesses/pi
RUN ln -s /opt/ezbx/harnesses/pi/node_modules/.bin/pi /usr/local/bin/pi \
 && pi --version

ENV PI_SKIP_VERSION_CHECK=1
USER agent
WORKDIR /workspace
LABEL ezbx.managed="true"
CMD ["bash"]
