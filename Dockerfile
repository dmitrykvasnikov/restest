# Build stage. Pinned to the same Go version the module asks for; the build is
# static so that the result can run on an image with no libc.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first: this layer is rebuilt only when go.mod or go.sum change,
# not on every source edit.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The .git directory is not in the build context, so Go cannot stamp the commit
# itself; `make up` passes it in. Without it the binary reports "unknown", which
# is honest rather than wrong.
ARG REVISION=""

# -trimpath keeps build paths out of the binary; -s -w drop the symbol and DWARF
# tables, which are of no use in a container.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.stampedRevision=${REVISION}" \
        -o /out/restest ./cmd/restest

# Runtime stage. distroless/static has no shell, no package manager and no libc:
# nothing to exploit, and nothing to keep patched. It does carry CA certificates
# and /etc/passwd, which is all a static Go binary needs.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/restest /restest

# Unprivileged by default; the image defines the nonroot user for us.
USER nonroot:nonroot
EXPOSE 8080

# No shell in the image, so this is exec form of necessity as well as of habit:
# the binary is PID 1 and receives SIGTERM directly, which is what its graceful
# shutdown is waiting for.
ENTRYPOINT ["/restest"]
