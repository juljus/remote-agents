BINDIR ?= $(HOME)/.local/bin
LOGDIR ?= $(HOME)/Library/Logs/remote-agents
PLIST  := bin/com.remote-agents.link.plist

.PHONY: build test lint install plist deploy-box e2e

build:
	go build -o bin/remote-agents ./cmd/remote-agents
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/remote-agents-box ./cmd/remote-agents-box

test:
	go test -race ./...

lint:
	go vet ./... && go vet -tags e2e ./e2e && staticcheck ./... && shellcheck box/install.sh box/bootstrap/*.sh box/bootstrap/host/*.sh box/bootstrap/user/*.sh scripts/deploy-box.sh testdata/fake-claude

# Installs the laptop binary and its rclaude entry point. Loads nothing.
install: build plist
	install -d $(BINDIR) $(LOGDIR)
	install -m 0755 bin/remote-agents $(BINDIR)/remote-agents
	ln -sfn remote-agents $(BINDIR)/rclaude

plist:
	install -d bin
	sed -e 's|@BINDIR@|$(BINDIR)|g' -e 's|@LOGDIR@|$(LOGDIR)|g' launchd/com.remote-agents.link.plist.in > $(PLIST)
	plutil -lint $(PLIST)

deploy-box:
	@test -n "$(HOST)" || { echo "usage: make deploy-box HOST=<ssh host>"; exit 1; }
	scripts/deploy-box.sh $(HOST) remote-agents

e2e:
	@test -n "$(HOST)" || { echo "usage: make e2e HOST=<ssh host>"; exit 1; }
	RA_E2E_HOST=$(HOST) go test -tags e2e -count=1 -timeout 280s -v ./e2e/
