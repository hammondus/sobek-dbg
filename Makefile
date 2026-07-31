# sobekdbg — DAP step-debugger experiment for sobek-embedded scripts.
.PHONY: build test run run-events release clean docker-build deploy logs install-ext

BIN := bin/demo
# Static, stripped, reproducible-path release binaries.
RELEASE_FLAGS := -trimpath -ldflags "-s -w"

# examples/ is a separate module (see DESIGN-DECISIONS.md "Repo layout"), so
# it needs its own go commands — the root ./... does not reach into it.
build:
	go build -o $(BIN) ./cmd/demo
	cd examples && go build -o ../bin/events ./events

test:
	go vet ./...
	go test -race ./...
	cd examples && go vet ./... && go test -race ./...

run: build
	# -wait holds the script until VS Code attaches (F5 in this workspace).
	./$(BIN) -wait testdata/sample.js

# Same attach config, same port — run one at a time.
run-events: build
	./bin/events -wait

release:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build $(RELEASE_FLAGS) -o dist/demo-darwin-arm64   ./cmd/demo
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build $(RELEASE_FLAGS) -o dist/demo-linux-arm64    ./cmd/demo
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build $(RELEASE_FLAGS) -o dist/demo-linux-amd64    ./cmd/demo
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(RELEASE_FLAGS) -o dist/demo-windows-amd64.exe ./cmd/demo

clean:
	rm -rf bin dist

# This is a local experiment with no server deployment; the docker/deploy
# targets exist to keep the command interface identical across repos.
docker-build:
	@echo "n/a: no docker component in this experiment"

deploy:
	@echo "n/a: no deploy target in this experiment"

logs:
	@echo "n/a: no service logs in this experiment"

# Install the tiny debug-type extension into VS Code (copy, not symlink, so
# deleting this repo doesn't break the editor).
install-ext:
	rm -rf "$(HOME)/.vscode/extensions/local.goja-dap-0.0.1" "$(HOME)/.vscode/extensions/local.sobekdbg-0.0.1"
	cp -r vscode-ext "$(HOME)/.vscode/extensions/local.sobekdbg-0.0.1"
	@echo "installed — reload VS Code windows to pick it up"
