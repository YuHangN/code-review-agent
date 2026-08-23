FROM golang:1.26.0-bookworm AS tools

RUN GOBIN=/out go install honnef.co/go/tools/cmd/staticcheck@v0.7.0

FROM golang:1.26.0-bookworm

COPY --from=tools /out/staticcheck /usr/local/bin/staticcheck

ENTRYPOINT []
