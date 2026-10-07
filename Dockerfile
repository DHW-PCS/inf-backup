FROM --platform=$BUILDPLATFORM golang:1.25.0-alpine AS build

ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/inf-backup ./cmd/inf-backup

FROM alpine:3.23.5

LABEL author="DHW PCS Maintainers" maintainer="maintainers@dhw.one"

LABEL org.opencontainers.image.source="https://github.com/DHW-PCS/inf-backup"
LABEL org.opencontainers.image.version="3.0.0"

RUN apk add --update --no-cache ca-certificates tzdata restic rclone
RUN adduser -D -h /home/container container

COPY --from=build /out/inf-backup /usr/local/bin/inf-backup
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/licenses/inf-backup/
USER container
ENV USER=container HOME=/home/container
WORKDIR /home/container
COPY --chmod=755 ./entrypoint-posix.sh /entrypoint.sh
ENTRYPOINT [ "/bin/ash", "/entrypoint.sh" ]
