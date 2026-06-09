FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/local-share .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/local-share /local-share
EXPOSE 8080
ENV LOCAL_SHARE_ADDR=:8080
ENTRYPOINT ["/local-share"]
