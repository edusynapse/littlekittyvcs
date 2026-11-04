FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/littlekittyvcs ./cmd/littlekittyvcs

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/littlekittyvcs /littlekittyvcs
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/littlekittyvcs"]
CMD ["serve"]
