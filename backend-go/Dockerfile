# Imagen del backend Go del POC. Contexto de build: este directorio.
FROM golang:1.27 AS build
WORKDIR /src

COPY . .
# Build hermetico contra vendor/: no se toca la red desde dentro del contenedor.
# Es lo que hace viable construir detras del proxy corporativo, cuya CA no esta
# en el almacen de la imagen base. Si se anade una dependencia hay que volver a
# ejecutar `go mod vendor` en el host.
#
# Sin cgo el binario es estatico y corre en una imagen sin libc.
RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/wspoc-go .

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
