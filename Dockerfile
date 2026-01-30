FROM golang:1.25 AS base
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /nextdns-sync
RUN apt update && apt install -y ca-certificates


FROM scratch
COPY --from=base /nextdns-sync /nextdns-sync
# We need certs to make https requests
COPY --from=base /etc/ssl /etc/ssl
CMD ["/nextdns-sync"]
