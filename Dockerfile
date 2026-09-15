# SPDX-License-Identifier: AGPL-3.0-or-later

FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# A static binary, so the runtime image needs no libc. The SQLite driver is
# modernc.org/sqlite precisely so this works with CGO off (ADR-0018).
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/nuno ./cmd/nuno

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nuno /usr/local/bin/nuno

# /data holds the SQLite file and its pre-migration backups. It must be
# writable by uid 65532, which is what distroless nonroot runs as.
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/nuno"]
CMD ["serve"]
