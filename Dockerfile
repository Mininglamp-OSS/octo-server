# YUJ-432 Round-3 · OCTO OSS 专用 Dockerfile
# (overlays over internal dmworkim/Dockerfile during release)
#
# Delta vs internal:
#   - `git describe --tags --abbrev=0` → fallback to "dev" when no tags exist.
#     OSS repos start fresh via octo-release (history-preserving mode) with
#     tags stripped, so `git describe` fails with "No names found" on first
#     build. The `2>/dev/null || echo dev` guard keeps the build green for
#     OSS users without sacrificing the real version embed on internal builds.

FROM golang:1.25 AS build

ENV GOPROXY https://goproxy.cn,direct
ENV GO111MODULE on

WORKDIR /go/cache

COPY go.mod .
COPY go.sum .
RUN go mod download

WORKDIR /go/release

COPY . .

RUN GIT_COMMIT=$(git rev-parse HEAD 2>/dev/null || echo unknown) && \
    GIT_COMMIT_DATE=$(git log --date=iso8601-strict -1 --pretty=%ct 2>/dev/null || echo 0) && \
    GIT_VERSION=$(git describe --tags --abbrev=0 2>/dev/null || echo dev) && \
    GIT_TREE_STATE=$(test -n "`git status --porcelain 2>/dev/null`" && echo "dirty" || echo "clean") && \
    CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-w -extldflags '-static' -X main.Commit=$GIT_COMMIT -X main.CommitDate=$GIT_COMMIT_DATE -X main.Version=$GIT_VERSION -X main.TreeState=$GIT_TREE_STATE" \
      -installsuffix cgo -o app ./main.go


FROM alpine:3.21 AS prod
COPY --from=build /etc/passwd /etc/passwd
COPY --from=build /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN mkdir -p /usr/share/zoneinfo/Asia && \
    ln -s /etc/localtime /usr/share/zoneinfo/Asia/Shanghai
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
WORKDIR /home
COPY --from=build /go/release/app /home
COPY --from=build /go/release/assets /home/assets
COPY --from=build /go/release/configs /home/configs
RUN echo "Asia/Shanghai" > /etc/timezone
ENV TZ=Asia/Shanghai

ENTRYPOINT ["/home/app"]
