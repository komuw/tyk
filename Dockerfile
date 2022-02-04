FROM golang:1.16 as builder

WORKDIR /build
COPY . .
RUN go mod tidy
RUN go build -o tyk

FROM ubuntu:20.04

COPY --from=builder /build/tyk /opt/tyk-gateway/tyk
COPY --from=builder /build/apps /opt/tyk-gateway/apps
COPY --from=builder /build/coprocess /opt/tyk-gateway/coprocess
COPY --from=builder /build/event_handlers /opt/tyk-gateway/event_handlers
COPY --from=builder /build/install /opt/tyk-gateway/install
COPY --from=builder /build/middleware /opt/tyk-gateway/middleware
COPY --from=builder /build/policies /opt/tyk-gateway/policies
COPY --from=builder /build/templates /opt/tyk-gateway/templates

EXPOSE 8080
WORKDIR /opt/tyk-gateway/

RUN mkdir -p /opt/tyk-gateway/templates

ENTRYPOINT ["/opt/tyk-gateway/tyk" ]
CMD [ "--conf=/opt/tyk-gateway/tyk.conf" ]

