# First stage: build the Go binary
FROM golang:1.21-alpine AS builder

# Install make and other build dependencies
RUN apk --no-cache add make gcc musl-dev
# Set the Current Working Directory inside the container
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download all dependencies
RUN go mod download

# Copy the source code
COPY . .

# Build the Go app using the Makefile
RUN make quai-cpu-miner

# Second stage: create a smaller runtime image
FROM alpine:latest

# Install any dependencies required to run the application
RUN apk --no-cache add ca-certificates cpulimit


# Set the Current Working Directory inside the container
WORKDIR /root/

# Copy the binary from the builder stage
COPY --from=builder /app/build/bin/quai-cpu-miner ./build/bin/quai-cpu-miner
COPY --from=builder /app/VERSION ./VERSION
RUN mkdir ./config


# Ensure the binary has execute permissions
RUN chmod +x ./build/bin/quai-cpu-miner

# Command to run the executable
CMD ["cpulimit", "--limit", "2", "./build/bin/quai-cpu-miner"]
