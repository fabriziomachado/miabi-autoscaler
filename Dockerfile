# syntax=docker/dockerfile:1
FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go vet ./... && go test -count=1 ./...
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/miabi-autoscaler .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/miabi-autoscaler /miabi-autoscaler
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/miabi-autoscaler"]
