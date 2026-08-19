FROM golang:1.26.6-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server && \
    CGO_ENABLED=0 go build -o /out/worker ./cmd/worker

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -H app
USER app
COPY --from=build /out/server /server
COPY --from=build /out/worker /worker
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/server"]
