#!/usr/bin/env bash
set -e

echo "=== 1. Building Binaries ==="
go build -o bin/catalogsync ./cmd/catalogsync
go build -o bin/mocksupplier ./cmd/mocksupplier

cleanup() {
    if [ -n "$MOCK_PID" ]; then
        echo "Stopping mock supplier (PID: $MOCK_PID)..."
        kill "$MOCK_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

echo ""
echo "=== 2. Starting Mock Supplier (Normal Scenario) ==="
./bin/mocksupplier -port 8080 -scenario normal &
MOCK_PID=$!
sleep 1

rm -f catalog_demo.db*

echo ""
echo "=== 3. First Sync Run (Start) ==="
./bin/catalogsync -db catalog_demo.db start

echo ""
echo "=== 4. Checking Status & Catalog Products ==="
./bin/catalogsync -db catalog_demo.db status
./bin/catalogsync -db catalog_demo.db products -status active | head -n 15

echo ""
echo "=== 5. Testing Idempotency (Running exact same feed again) ==="
./bin/catalogsync -db catalog_demo.db start

echo ""
echo "=== 6. Simulating Supplier Outage & Resumability ==="
kill "$MOCK_PID" 2>/dev/null || true
sleep 1
# Outage after 2 batches
./bin/mocksupplier -port 8080 -scenario normal -outage-after 2 -outage-for 5m &
MOCK_PID=$!
sleep 1

rm -f outage_demo.db*
echo "Running sync until outage..."
set +e
./bin/catalogsync -db outage_demo.db -retries 2 -backoff 100ms start
set -e

echo ""
echo "Run was interrupted as expected. Checking status:"
./bin/catalogsync -db outage_demo.db status

echo ""
echo "Simulating supplier recovery..."
curl -s -X POST http://localhost:8080/admin/recover
echo ""

echo "Resuming interrupted run..."
./bin/catalogsync -db outage_demo.db resume
./bin/catalogsync -db outage_demo.db status

echo ""
echo "=== Demo Completed Successfully ==="

