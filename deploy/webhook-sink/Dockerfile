# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/webhook-sink .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/webhook-sink /webhook-sink
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/webhook-sink"]
