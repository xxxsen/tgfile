FROM golang:1.24

WORKDIR /build
COPY . ./
RUN CGO_ENABLED=0 go build -a -tags netgo -ldflags '-w' -o tgfile ./cmd

FROM alpine:3.12
COPY --from=0 /build/tgfile /bin/

ENTRYPOINT [ "/bin/tgfile" ]