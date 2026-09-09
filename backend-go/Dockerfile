# Imagen del backend Go del POC. Contexto de build: este directorio.
FROM golang:1.27 AS build
WORKDIR /src

# Las dependencias se descargan durante el build, asi que el contenedor necesita
# salida a la red. Detras del proxy corporativo hay que inyectar la CA y las
# variables de proxy al construir, por ejemplo:
#
#     docker build --build-arg HTTPS_PROXY=$env:HTTPS_PROXY -t wspoc-go .
#
# Se copian go.mod/go.sum antes que el codigo para que la capa de descarga se
# reutilice mientras no cambien las dependencias.
# CA corporativa para el build detras del proxy del banco, que intercepta TLS con
# bgsc24CA. Esa CA vive en el almacen de Windows (por eso `go mod download` va en
# el host) pero no en la imagen base Linux. Exporta el .crt a certs/ antes de
# construir; el directorio solo lleva .gitkeep en el repo, asi que en CI este
# COPY no aporta nada y update-ca-certificates es un no-op.
# COPY certs/ /usr/local/share/ca-certificates/
# RUN update-ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Sin cgo el binario es estatico y corre en una imagen sin libc.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/wspoc-go ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/wspoc-go /app/wspoc-go

# No hay .env dentro de la imagen: la configuracion entra por variables de
# entorno, que en este POC tienen prioridad sobre el fichero (config.go).
# WS_JWT_SECRET es obligatorio y el proceso aborta si falta o mide menos de 32 bytes.
ENV WS_PORT=8080 \
    WS_BIND_ADDRESS=0.0.0.0

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/wspoc-go"]
