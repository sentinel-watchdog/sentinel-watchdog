# Tools to inspect and lint the packages in dist/ (task package-verify,
# task package-lint; D-102). Verification only: nothing here ships. BASE is
# Taskfile.yml's PACKAGE_IMAGE_DEBIAN, pinned by digest; the tools come from
# Debian 13's archive.
ARG BASE
FROM ${BASE}
RUN apt-get update \
	&& apt-get install -y --no-install-recommends file rpm lintian rpmlint \
	&& rm -rf /var/lib/apt/lists/*
