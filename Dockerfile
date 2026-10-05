FROM node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 AS web
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY web ./web
RUN npm run build

FROM golang:1.26.8-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /src/internal/filemind/web/dist ./internal/filemind/web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /filemind ./cmd/filemind \
    && mkdir /data && chown 10001:10001 /data

FROM scratch
COPY --from=build /filemind /filemind
COPY --from=build --chown=10001:10001 /data /data
USER 10001:10001
EXPOSE 8080 8081
ENTRYPOINT ["/filemind"]
