FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /embewi-core ./cmd/controller/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /embewi-core /embewi-core
ENTRYPOINT ["/embewi-core"]
