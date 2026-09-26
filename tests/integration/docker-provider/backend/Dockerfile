FROM golang:1.25-alpine AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -o /backend main.go

FROM alpine:3.22
COPY --from=build /backend /backend
EXPOSE 8443
ENTRYPOINT ["/backend"]
