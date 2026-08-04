FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -o /expenses .

FROM alpine:3.21
WORKDIR /app
COPY --from=builder /expenses /app/expenses
COPY web /app/web
ENV PORT=8080
EXPOSE 8080
CMD ["/app/expenses"]
