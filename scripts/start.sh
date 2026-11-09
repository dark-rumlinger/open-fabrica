#!/bin/bash
echo "Starting Open Fabrica..."
echo "Admin panel: http://localhost:5734"
echo "Public project:  http://localhost:5733"
echo ""

BINARY="./openfabrica"
if [ ! -f "$BINARY" ]; then
    echo "Building..."
    go build -o "$BINARY" ./cmd/openfabrica || { echo "Build failed!"; exit 1; }
fi

exec "$BINARY"
