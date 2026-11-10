# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/jahrulnr/fakeagents/internal/version.Version=${VERSION} -X github.com/jahrulnr/fakeagents/internal/version.Commit=${COMMIT}" \
    -o /out/fakeagents ./cmd/fakeagents

FROM alpine:3.22
RUN apk add --no-cache ca-certificates wget \
    && addgroup -S -g 10001 fakeagents \
    && adduser -S -D -H -u 10001 -G fakeagents fakeagents
COPY --from=build /out/fakeagents /usr/local/bin/fakeagents
USER fakeagents
EXPOSE 11110
ENTRYPOINT ["fakeagents"]
CMD ["serve"]
