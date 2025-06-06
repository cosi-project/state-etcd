# syntax = docker/dockerfile-upstream:1.14.1-labs

# THIS FILE WAS AUTOMATICALLY GENERATED, PLEASE DO NOT EDIT.
#
# Generated on 2025-04-22T10:59:51Z by kres fd5cab0.

ARG TOOLCHAIN

# cleaned up specs and compiled versions
FROM scratch AS generate

# runs markdownlint
FROM docker.io/oven/bun:1.2.9-alpine AS lint-markdown
WORKDIR /src
RUN bun i markdownlint-cli@0.44.0 sentences-per-line@0.3.0
COPY .markdownlint.json .
COPY ./README.md ./README.md
RUN bunx markdownlint --ignore "CHANGELOG.md" --ignore "**/node_modules/**" --ignore '**/hack/chglog/**' --rules sentences-per-line .

# base toolchain image
FROM --platform=${BUILDPLATFORM} ${TOOLCHAIN} AS toolchain
RUN apk --update --no-cache add bash curl build-base protoc protobuf-dev

# build tools
FROM --platform=${BUILDPLATFORM} toolchain AS tools
ENV GO111MODULE=on
ARG CGO_ENABLED
ENV CGO_ENABLED=${CGO_ENABLED}
ARG GOTOOLCHAIN
ENV GOTOOLCHAIN=${GOTOOLCHAIN}
ARG GOEXPERIMENT
ENV GOEXPERIMENT=${GOEXPERIMENT}
ENV GOPATH=/go
ARG DEEPCOPY_VERSION
RUN --mount=type=cache,target=/root/.cache/go-build,id=state-etcd/root/.cache/go-build --mount=type=cache,target=/go/pkg,id=state-etcd/go/pkg go install github.com/siderolabs/deep-copy@${DEEPCOPY_VERSION} \
	&& mv /go/bin/deep-copy /bin/deep-copy
ARG GOLANGCILINT_VERSION
RUN --mount=type=cache,target=/root/.cache/go-build,id=state-etcd/root/.cache/go-build --mount=type=cache,target=/go/pkg,id=state-etcd/go/pkg go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCILINT_VERSION} \
	&& mv /go/bin/golangci-lint /bin/golangci-lint
RUN --mount=type=cache,target=/root/.cache/go-build,id=state-etcd/root/.cache/go-build --mount=type=cache,target=/go/pkg,id=state-etcd/go/pkg go install golang.org/x/vuln/cmd/govulncheck@latest \
	&& mv /go/bin/govulncheck /bin/govulncheck
ARG GOFUMPT_VERSION
RUN go install mvdan.cc/gofumpt@${GOFUMPT_VERSION} \
	&& mv /go/bin/gofumpt /bin/gofumpt

# tools and sources
FROM tools AS base
WORKDIR /src
COPY go.mod go.mod
COPY go.sum go.sum
RUN cd .
RUN --mount=type=cache,target=/go/pkg,id=state-etcd/go/pkg go mod download
RUN --mount=type=cache,target=/go/pkg,id=state-etcd/go/pkg go mod verify
COPY ./pkg ./pkg
RUN --mount=type=cache,target=/go/pkg,id=state-etcd/go/pkg go list -mod=readonly all >/dev/null

# runs gofumpt
FROM base AS lint-gofumpt
RUN FILES="$(gofumpt -l .)" && test -z "${FILES}" || (echo -e "Source code is not formatted with 'gofumpt -w .':\n${FILES}"; exit 1)

# runs golangci-lint
FROM base AS lint-golangci-lint
WORKDIR /src
COPY .golangci.yml .
ENV GOGC=50
RUN --mount=type=cache,target=/root/.cache/go-build,id=state-etcd/root/.cache/go-build --mount=type=cache,target=/root/.cache/golangci-lint,id=state-etcd/root/.cache/golangci-lint,sharing=locked --mount=type=cache,target=/go/pkg,id=state-etcd/go/pkg golangci-lint run --config .golangci.yml

# runs govulncheck
FROM base AS lint-govulncheck
WORKDIR /src
RUN --mount=type=cache,target=/root/.cache/go-build,id=state-etcd/root/.cache/go-build --mount=type=cache,target=/go/pkg,id=state-etcd/go/pkg govulncheck ./...

# runs unit-tests with race detector
FROM base AS unit-tests-race
WORKDIR /src
ARG TESTPKGS
RUN --mount=type=cache,target=/root/.cache/go-build,id=state-etcd/root/.cache/go-build --mount=type=cache,target=/go/pkg,id=state-etcd/go/pkg --mount=type=cache,target=/tmp,id=state-etcd/tmp CGO_ENABLED=1 go test -v -race -count 1 ${TESTPKGS}

# runs unit-tests
FROM base AS unit-tests-run
WORKDIR /src
ARG TESTPKGS
RUN --mount=type=cache,target=/root/.cache/go-build,id=state-etcd/root/.cache/go-build --mount=type=cache,target=/go/pkg,id=state-etcd/go/pkg --mount=type=cache,target=/tmp,id=state-etcd/tmp go test -v -covermode=atomic -coverprofile=coverage.txt -coverpkg=${TESTPKGS} -count 1 ${TESTPKGS}

FROM scratch AS unit-tests
COPY --from=unit-tests-run /src/coverage.txt /coverage-unit-tests.txt

