# syntax=docker/dockerfile:1
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

ARG BASE_IMAGE

FROM rust:1.98.0-bookworm AS jcode-builder

ARG JCODE_VERSION=v0.58.0
ARG JCODE_COMMIT=5e064af4a57bf1f1a4e9d68f283a2bb1a2ec09bd

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    clang \
    git \
    libssl-dev \
    pkg-config \
  && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 --branch "${JCODE_VERSION}" \
      https://github.com/1jehuang/jcode.git /src/jcode \
  && test "$(git -C /src/jcode rev-parse HEAD)" = "${JCODE_COMMIT}"

WORKDIR /src/jcode

# Keep upstream's optional default feature set out of the pilot build. Runtime
# RSS still needs to be measured under representative concurrent workloads.
ARG TARGETARCH
RUN --mount=type=cache,id=jcode-cargo-registry,target=/usr/local/cargo/registry,sharing=locked \
    --mount=type=cache,id=jcode-cargo-git,target=/usr/local/cargo/git,sharing=locked \
    --mount=type=cache,id=jcode-target-${TARGETARCH},target=/src/jcode/target,sharing=locked \
    JCODE_RELEASE_BUILD=1 JCODE_BUILD_SEMVER="${JCODE_VERSION#v}" \
    cargo build --locked --release --no-default-features -p jcode --bin jcode \
  && install -Dm755 target/release/jcode /out/jcode

FROM ${BASE_IMAGE}

COPY --from=jcode-builder /out/jcode /usr/local/bin/jcode

ENV JCODE_NO_TELEMETRY=1

RUN mkdir -p /home/scion/.jcode/skills /home/scion/.config/jcode \
  && chown -R scion:scion /home/scion/.jcode /home/scion/.config/jcode

CMD ["jcode"]
