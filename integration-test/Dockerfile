# Multi-stage build for Caddy with FrankenPHP and Sidekick module
FROM dunglas/frankenphp:php8.3-bookworm AS builder

ARG GO_VERSION
ARG XCADDY_VERSION
ARG CADDY_VERSION
ARG FRANKENPHP_CADDY_VERSION

# Install build dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates     \
    curl                \
    git                 \
    && apt-get clean    \
    && rm -rf /var/lib/apt/lists/*

# Install xcaddy
WORKDIR /tmp
RUN curl --silent -Lo - https://github.com/caddyserver/xcaddy/releases/download/v${XCADDY_VERSION}/xcaddy_${XCADDY_VERSION}_linux_amd64.tar.gz | tar -zxf - && install -D /tmp/xcaddy /usr/local/bin/xcaddy
RUN curl --silent -Lo - https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz | tar -zxf - && mv go /usr/local/go

ENV PATH=${PATH}:/root/go/bin:/usr/local/go/bin
ENV GOROOT=/usr/local/go

# Set working directory
WORKDIR /build

# Copy the sidekick module source (context is parent directory)
COPY . /sidekick

# Libs needed for building Caddy
RUN apt-get update && apt-get install -y --no-install-recommends \
    libcurl4-openssl-dev    \
    libxml2-dev             \
    libreadline-dev         \
    libncurses-dev          \
    libargon2-dev           \
    libssl-dev              \
    zlib1g-dev              \
    libsqlite3-dev          \
    libonig-dev             \
    && apt-get clean        \
    && rm -rf /var/lib/apt/lists/*

# Build Caddy with FrankenPHP and Sidekick module
RUN CGO_ENABLED=1 XCADDY_SETCAP=1 XCADDY_GO_BUILD_FLAGS='-ldflags="-w -s" -tags=nobadger,nomysql,nopgx,nowatcher' CGO_CFLAGS=$(php-config --includes) CGO_LDFLAGS="$(php-config --ldflags) $(php-config --libs)" xcaddy build v${CADDY_VERSION} \
    --with github.com/dunglas/frankenphp/caddy@v${FRANKENPHP_CADDY_VERSION} \
    --with github.com/honest-hosting/caddy-sidekick=/sidekick               \
    --output /usr/local/bin/caddy

# Final stage - minimal runtime image
FROM dunglas/frankenphp:php8.3-bookworm

# Copy the built Caddy binary from builder
COPY --from=builder /usr/local/bin/caddy /usr/local/bin/caddy

# Create necessary directories
RUN mkdir -p /var/www/html /var/cache/sidekick /etc/caddy

# Set working directory
WORKDIR /var/www/html

# Expose ports
EXPOSE 80 443

# Health check
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=3 \
    CMD curl -f http://localhost/health.php || exit 1

# Run Caddy with FrankenPHP
CMD ["caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
