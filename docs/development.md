# Build and test without Docker

[Back to the overview](../README.md)

The vendor table is not part of the repository. Fetch it once before the first
build, also before building the Docker image:

    go run ./cmd/oui-update

Then:

    go build -o kea-roost ./cmd/kea-roost
    go test ./...

To work on the interface without a real server, run it against the test double:

    go run ./cmd/fake-kea -listen 127.0.0.1:8000 &
    KEA_URL=http://127.0.0.1:8000 MANAGER_USER=admin MANAGER_PASSWORD=admin \
      MANAGER_DATA_DIR=./data go run ./cmd/kea-roost

To also run the browser tests, which need a local Chromium or Chrome:

    go test -tags e2e ./internal/web

The integration tests run the manager against the real `kea-dhcp4` binary. Each
test starts a private server with a synthetic configuration, without any DHCP
interface, and restarts it to check what was stored. They are skipped if Kea is not
installed:

    go test -tags kea ./internal/manager

The screenshots above are written by a test from invented data:

    SCREENSHOT_DIR=docs/screenshots go test -tags e2e -run TestScreenshots ./internal/web
