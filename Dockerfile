FROM golang:1.26-alpine AS build
WORKDIR /src/core
# 国内网络：模块代理对齐宿主机（goproxy.cn）；离线/内网构建可改自己的镜像代理。
ENV GOPROXY=https://goproxy.cn,direct
RUN apk add --no-cache git
# go.mod 的 replace 指向 ../.hwhkit-go-src —— 保持与仓库内相同的相对层级：
# /src/core/go.mod → ../ = /src/.hwhkit-go-src
COPY core/go.mod core/go.sum ./
COPY .hwhkit-go-src /src/.hwhkit-go-src
RUN go mod download
COPY core/cmd ./cmd
COPY core/internal ./internal
COPY core/web ./web
RUN CGO_ENABLED=0 go build -trimpath -o /out/ir-agent-server ./cmd/ir-agent-server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata curl jq
WORKDIR /app
COPY --from=build /out/ir-agent-server ./ir-agent-server
COPY core/config ./config
EXPOSE 8080
ENTRYPOINT ["./ir-agent-server"]
