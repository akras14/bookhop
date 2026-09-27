BIN     := bookhop
DIST    := dist
LDFLAGS := -s -w
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: all build darwin-arm64 darwin-amd64 windows-amd64 vet clean

all: darwin-arm64 darwin-amd64 windows-amd64

# Build for the current machine.
build:
	$(GOBUILD) -o $(BIN) .

darwin-arm64:
	GOOS=darwin GOARCH=arm64 $(GOBUILD) -o $(DIST)/$(BIN)-darwin-arm64 .

darwin-amd64:
	GOOS=darwin GOARCH=amd64 $(GOBUILD) -o $(DIST)/$(BIN)-darwin-amd64 .

windows-amd64:
	GOOS=windows GOARCH=amd64 $(GOBUILD) -o $(DIST)/$(BIN).exe .

vet:
	go vet ./...

clean:
	rm -rf $(DIST) $(BIN)
