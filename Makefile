.PHONY: build check release dist bump
VERSION := $(shell cat VERSION)
FLAGS := -s -w -X github.com/LIghtJUNction/zcalld-compat/internal/buildinfo.Version=$(VERSION)
build:
	go build -mod=readonly -trimpath -ldflags "$(FLAGS)" -o bin/zcalld-privacy ./cmd/zcalld-privacy
	go build -mod=readonly -trimpath -ldflags "$(FLAGS)" -o bin/zcalld-pluginctl ./cmd/zcalld-pluginctl
check:
	go test -mod=readonly ./privacy ./community ./builtin ./internal/privatefile ./internal/redact ./internal/lazyre ./internal/pii/guard
	go vet -mod=readonly ./privacy ./community ./builtin ./internal/privatefile ./cmd/...
	bun test community/builtin/tests
	python3 -c "import ast,pathlib; [ast.parse(p.read_text()) for p in pathlib.Path('scripts').glob('*.py')]"
dist:
	python3 scripts/release.py build --target all
	python3 scripts/release.py stage
release: check
	python3 scripts/release.py tag
bump:
	python3 scripts/bump.py $(PART)
