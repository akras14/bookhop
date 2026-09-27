BIN     := bookhop
NAME    := Send Books to iPhone
VERSION := 1.0.0
DIST    := dist
LDFLAGS := -s -w
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"
WINRES  := go run github.com/tc-hib/go-winres@v0.3.3
APP     := $(DIST)/$(NAME).app

.PHONY: all build darwin-arm64 darwin-amd64 windows-amd64 mac-app icons release vet clean

all: darwin-arm64 darwin-amd64 windows-amd64 mac-app

# Build for the current machine (CLI use: bookhop apps / send / ls).
build:
	$(GOBUILD) -o $(BIN) .

darwin-arm64:
	GOOS=darwin GOARCH=arm64 $(GOBUILD) -o $(DIST)/$(BIN)-darwin-arm64 .

darwin-amd64:
	GOOS=darwin GOARCH=amd64 $(GOBUILD) -o $(DIST)/$(BIN)-darwin-amd64 .

# Windowed build: no console window, just the browser page. The icon and
# "Send Books to iPhone" name come from rsrc_windows_amd64.syso (make icons).
windows-amd64:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS) -H windowsgui" -o $(DIST)/$(BIN).exe .

# "Send Books to iPhone.app": universal binary + icon, ad-hoc signed.
mac-app: darwin-arm64 darwin-amd64
	rm -rf "$(APP)"
	mkdir -p "$(APP)/Contents/MacOS" "$(APP)/Contents/Resources"
	lipo -create -output "$(APP)/Contents/MacOS/$(BIN)" $(DIST)/$(BIN)-darwin-arm64 $(DIST)/$(BIN)-darwin-amd64
	cp assets/Info.plist "$(APP)/Contents/Info.plist"
	cp assets/AppIcon.icns "$(APP)/Contents/Resources/AppIcon.icns"
	codesign --force --sign - "$(APP)"

# Regenerate every icon from assets/icon-source.jpg. Outputs are committed so
# a plain `go build` picks up the Windows icon without running this.
icons:
	go run ./tools/mkicon -src assets/icon-source.jpg -iconset build/AppIcon.iconset
	iconutil -c icns build/AppIcon.iconset -o assets/AppIcon.icns
	$(WINRES) simply --arch amd64 --out rsrc --icon assets/icon.png --manifest gui \
		--product-name "$(NAME)" --file-description "$(NAME)" \
		--product-version $(VERSION) --file-version $(VERSION) \
		--original-filename $(BIN).exe

# Publish dist/ builds as GitHub release v$(VERSION). Bump VERSION first;
# the tag is created on the current commit, so commit and push before this.
release: clean all
	cd $(DIST) && ditto -c -k --keepParent "$(NAME).app" "Send-Books-to-iPhone-mac.zip"
	gh release create v$(VERSION) --title "$(NAME) $(VERSION)" --generate-notes \
		$(DIST)/$(BIN).exe \
		$(DIST)/Send-Books-to-iPhone-mac.zip \
		$(DIST)/$(BIN)-darwin-arm64 \
		$(DIST)/$(BIN)-darwin-amd64

vet:
	go vet ./...

clean:
	rm -rf $(DIST) $(BIN) build
