FROM golang:1.23-alpine AS build
WORKDIR /src
ENV GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/kubergo ./cmd/kubergo

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/kubergo /kubergo

EXPOSE 8080
ENV PORT=8080

USER nonroot:nonroot
ENTRYPOINT ["/kubergo"]
