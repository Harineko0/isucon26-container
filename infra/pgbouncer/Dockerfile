FROM alpine:3.22 AS build
RUN apk add --no-cache build-base curl libevent-dev libevent-static openssl-dev openssl-libs-static
WORKDIR /src
RUN curl -fsSLo pgbouncer.tar.gz https://www.pgbouncer.org/downloads/files/1.26.1/pgbouncer-1.26.1.tar.gz \
 && echo '00886a2994b4844cd37110970df777772cfc42a74c400522c82fba509a3f42d9  pgbouncer.tar.gz' | sha256sum -c - \
 && tar xf pgbouncer.tar.gz --strip-components=1 \
 && LDFLAGS=-static ./configure --prefix=/usr --without-cares \
 && make -j2 pgbouncer && strip pgbouncer && ! readelf -l pgbouncer | grep -q INTERP

FROM scratch
COPY --from=build /src/pgbouncer /pgbouncer-1.26.1
