FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /pdfbot .

# static + ca-сертификаты + /tmp, uid 65532
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pdfbot /pdfbot
EXPOSE 8080
ENTRYPOINT ["/pdfbot"]
