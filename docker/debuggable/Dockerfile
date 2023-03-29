FROM golang:latest AS getter
RUN GOARCH=amd64 go install github.com/go-delve/delve/cmd/dlv@latest
#COPY dlv    .

FROM docker.io/therealmitchconnors/pilot:ambient

EXPOSE 15018
COPY --from=0 /go/bin/linux_amd64/dlv .
ENTRYPOINT ["./dlv", "--listen=:40000", "--headless=true", "--api-version=2", "exec", "/usr/local/bin/pilot-discovery", "--"]
