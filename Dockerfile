FROM golang:1.26.4-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/reimbursement-api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -o /out/reimbursement-worker ./cmd/worker \
 && CGO_ENABLED=0 go build -trimpath -o /out/reimbursement-migrate ./cmd/migrate \
 && CGO_ENABLED=0 go build -trimpath -o /out/reimbursement-authz-catalog ./cmd/authz-catalog

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S reimbursement && adduser -S -G reimbursement reimbursement \
 && mkdir -p /var/lib/commercial-license \
 && chown reimbursement:reimbursement /var/lib/commercial-license
WORKDIR /app
COPY --from=builder /out/ /app/
USER reimbursement
EXPOSE 8087
ENTRYPOINT ["/app/reimbursement-api"]
