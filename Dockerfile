FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /pdfbot .

# static + ca-сертификаты + /tmp, uid 65532
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pdfbot /pdfbot
# лимит пода 256 МБ: GC начинает поджимать кучу заранее, а не ловит OOM
ENV GOMEMLIMIT=200MiB
EXPOSE 8080
ENTRYPOINT ["/pdfbot"]
