FROM golang:1.25 AS base
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /nextdns-sync
RUN apt-get update & \
    apt-get install --no-install-recommends -y ca-certificates

FROM scratch
COPY --from=base /nextdns-sync /nextdns-sync
# We need certs to make https requests
COPY --from=base /etc/ssl /etc/ssl
COPY --from=base /etc/passwd /etc/passwd
USER nobody
CMD ["/nextdns-sync"]
