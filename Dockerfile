FROM golang:1.26.11-alpine@sha256:ae52b5bc6ed045a9ed26ab03dc19327a802a9cc9aa8b27ed57f5c6b60579b0bb AS builder
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
