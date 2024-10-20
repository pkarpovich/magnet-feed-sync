ARG GO_VERSION=1.23.2
ARG NODE_VERSION=20.18.0
ARG PNPM_VERSION=9.12

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

ARG TARGETARCH

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,target=. \
    CGO_ENABLED=0 GOARCH=$TARGETARCH go build -o /bin/server ./app


FROM node:${NODE_VERSION}-alpine as base

WORKDIR /usr/src/app

RUN --mount=type=cache,target=/root/.npm \
    npm install -g pnpm@${PNPM_VERSION}


FROM base as deps

RUN --mount=type=bind,source=./frontend/package.json,target=package.json \
    --mount=type=bind,source=./frontend/pnpm-lock.yaml,target=pnpm-lock.yaml \
    --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --prod --frozen-lockfile


FROM deps as frontend-build

RUN --mount=type=bind,source=./frontend/package.json,target=package.json \
    --mount=type=bind,source=./frontend/pnpm-lock.yaml,target=pnpm-lock.yaml \
    --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile

COPY frontend .
RUN pnpm run build


FROM alpine:latest AS final

RUN --mount=type=cache,target=/var/cache/apk \
    apk --update add \
        ca-certificates \
        tzdata \
        && \
        update-ca-certificates

COPY --from=build /bin/server /bin/
COPY --from=frontend-build /usr/src/app/dist /static

ENTRYPOINT [ "/bin/server" ]
