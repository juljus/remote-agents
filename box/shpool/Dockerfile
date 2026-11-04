# The pinned toolchain for build.sh: shpool's own rust-toolchain.toml channel plus the musl target.
FROM docker.io/library/rust:1.85.0-bookworm@sha256:0ff31c9ffa641a62e48d543fb00b4960955ea375f40776f40f585b89e654cc5e
RUN rustup target add x86_64-unknown-linux-musl
