FROM busybox:uclibc AS busybox

# Build image
FROM golang:1.22-alpine as build

WORKDIR /go/src/app

COPY . .
RUN if [ ! -f vendor/modules.txt ]; then go mod download; fi
RUN CGO_ENABLED=0 go build -o /go/bin/talonflame ./bin/talonflame
RUN CGO_ENABLED=0 go build -o /go/bin/talonflame-db-migrate ./bin/talonflame-db-migrate

# Now copy it into our base image.
FROM gcr.io/distroless/static-debian12 as runner
COPY --from=busybox /bin/wget /usr/bin/wget
COPY --from=build /go/bin/talonflame /go/bin/talonflame-db-migrate /talonflame/

WORKDIR /talonflame
CMD ["./talonflame"]
