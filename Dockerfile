FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/zulip-smtp .

# distroless/static, NOT scratch.
#
# ★ The relay makes an outbound HTTPS call to Zulip on every message, so it needs
# a CA bundle to verify Zulip's certificate. scratch has none, and the failure is
# an unhelpful x509 error at send time rather than anything at startup.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/zulip-smtp /usr/local/bin/zulip-smtp

USER nonroot:nonroot
EXPOSE 1025 8080

ENTRYPOINT ["/usr/local/bin/zulip-smtp"]
