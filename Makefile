BIN_DIR ?= $(HOME)/.local/bin
CONFIG_DIR ?= $(HOME)/.config/handoffd
AGENT := local.handoffd
PLIST := $(HOME)/Library/LaunchAgents/$(AGENT).plist
UNIT := $(HOME)/.config/systemd/user/handoffd.service
OS := $(shell uname -s | tr A-Z a-z)
ARCH := $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
GOOS ?= $(OS)
GOARCH ?= $(ARCH)

.PHONY: test build install configure uninstall start stop restart logs status

test:
	docker build --target test .

build: test
	docker build --build-arg GOOS=$(GOOS) --build-arg GOARCH=$(GOARCH) --target artifact --output type=local,dest=bin .

install: build
	install -d $(BIN_DIR) $(CONFIG_DIR)
	install -m 755 bin/handoffd $(BIN_DIR)/handoffd
	@if [ -f $(CONFIG_DIR)/config.json ]; then $(BIN_DIR)/handoffd setup-agent; echo "config kept; run 'make configure' to walk through the settings again"; else $(BIN_DIR)/handoffd init; fi
	$(MAKE) stop
ifeq ($(OS),darwin)
	install -d $(HOME)/Library/Logs
	sed -e 's|@HOME@|$(HOME)|g' deploy/handoffd.sb > $(CONFIG_DIR)/handoffd.sb
	sed -e 's|@HOME@|$(HOME)|g' deploy/$(AGENT).plist > $(PLIST)
	launchctl bootstrap gui/$(shell id -u) $(PLIST)
	launchctl print gui/$(shell id -u)/$(AGENT) | head -20
else
	install -d $(dir $(UNIT))
	install -m 644 deploy/handoffd.service $(UNIT)
	systemctl --user daemon-reload
	systemctl --user enable --now handoffd.service
	systemctl --user status --no-pager handoffd.service | head -20
endif

configure:
	$(BIN_DIR)/handoffd init
	$(MAKE) restart

uninstall: stop
ifeq ($(OS),darwin)
	rm -f $(PLIST) $(BIN_DIR)/handoffd
else
	-systemctl --user disable handoffd.service
	rm -f $(UNIT) $(BIN_DIR)/handoffd
	-systemctl --user daemon-reload
endif

start:
ifeq ($(OS),darwin)
	launchctl bootstrap gui/$(shell id -u) $(PLIST)
else
	systemctl --user start handoffd.service
endif

stop:
ifeq ($(OS),darwin)
	launchctl bootout gui/$(shell id -u)/$(AGENT) 2>/dev/null || true
else
	-systemctl --user stop handoffd.service 2>/dev/null
endif
	@for i in 1 2 3 4 5 6 7 8 9 10; do pgrep -f '$(BIN_DIR)/handoffd watch' >/dev/null || break; sleep 1; done

restart: stop start

status:
ifeq ($(OS),darwin)
	launchctl print gui/$(shell id -u)/$(AGENT) | head -30
else
	systemctl --user status --no-pager handoffd.service
endif

logs:
ifeq ($(OS),darwin)
	tail -f $(HOME)/Library/Logs/handoffd.log
else
	journalctl --user -u handoffd.service -f
endif
