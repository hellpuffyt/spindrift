FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /spindrift ./cmd/spindrift

FROM scratch
COPY --from=build /spindrift /spindrift
COPY examples /examples
EXPOSE 8080
ENTRYPOINT ["/spindrift"]
CMD ["serve", "/examples/spindrift.conf"]
