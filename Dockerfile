# leaks.pigfox.com: static Go binary on distroless, non-root.
# Both base images are pinned to tags already pulled on 104.223.116.10.
FROM golang:1.27.1 AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/leak-demo ./cmd/leak-demo

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG CREATED_AT=unknown
ARG CREATED_BY=docker-build
LABEL org.pigfox.estate="RN" \
      org.pigfox.created-by="${CREATED_BY}" \
      org.pigfox.created-at="${CREATED_AT}" \
      org.pigfox.purpose="Goroutine leak profile demo for pigfox.com/demos/goroutine-leaks" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/leak-demo /leak-demo
USER nonroot:nonroot
EXPOSE 8549
HEALTHCHECK --interval=15s --timeout=4s --start-period=15s --retries=3 CMD ["/leak-demo", "-healthcheck"]
ENTRYPOINT ["/leak-demo"]
