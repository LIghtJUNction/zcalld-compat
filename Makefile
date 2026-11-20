.PHONY: build check release dist bump
VERSION := $(shell cat VERSION)
FLAGS := -s -w -X github.com/LIghtJUNction/zcalld-compat/internal/buildinfo.Version=$(VERSION)
build:
	go build -mod=readonly -trimpath -ldflags "$(FLAGS)" -o bin/zcalld-privacy ./cmd/zcalld-privacy
	go build -mod=readonly -trimpath -ldflags "$(FLAGS)" -o bin/zcalld-pluginctl ./cmd/zcalld-pluginctl
	go build -mod=readonly -trimpath -ldflags "$(FLAGS)" -o bin/zcalld-claude-tui ./cmd/zcalld-claude-tui
check:
	go test -mod=readonly ./cmd/zcalld-privacy ./cmd/zcalld-claude-tui ./privacy ./community ./builtin ./routing ./desktop ./maintenance ./claudemaintenance ./internal/claudetui/... ./internal/privatefile ./internal/redact ./internal/lazyre ./internal/pii/guard
	go vet -mod=readonly ./privacy ./community ./builtin ./routing ./desktop ./maintenance ./claudemaintenance ./internal/claudetui/... ./internal/privatefile ./cmd/...
	bun test community/builtin/tests community/tests
	python3 -c "import ast,pathlib; [ast.parse(p.read_text()) for p in pathlib.Path('scripts').glob('*.py')]"
	python3 scripts/test-aur.py
	python3 scripts/test-package.py
	mkdir -p bin
	go build -mod=readonly -trimpath -ldflags "$(FLAGS)" -o bin/zcalld-pluginctl ./cmd/zcalld-pluginctl
	python3 scripts/test-maintenance.py bin/zcalld-pluginctl
	go build -mod=readonly -trimpath -ldflags "$(FLAGS)" -o bin/zcalld-claude-tui ./cmd/zcalld-claude-tui
	go test -mod=readonly -c -o bin/fake-claude-tui ./internal/claudetui
	python3 scripts/test-claude-tui.py --helper bin/zcalld-claude-tui --fake-claude bin/fake-claude-tui
	go test -mod=readonly -c -o bin/fake-claude-maintenance ./claudemaintenance
	python3 scripts/test-claude-maintenance.py --helper bin/zcalld-pluginctl --fake-claude bin/fake-claude-maintenance
dist:
	python3 scripts/release.py build --target all
	python3 scripts/release.py stage
release: check
	python3 scripts/release.py tag
bump:
	python3 scripts/bump.py $(PART)
