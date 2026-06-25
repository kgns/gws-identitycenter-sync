.PHONY: build test vet tidy clean

# Local dev build.
build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# Invoked by `sam build` (BuildMethod: makefile). Produces the Lambda `bootstrap`
# binary for the custom (provided.al2023) runtime.
build-SyncFunction:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -o $(ARTIFACTS_DIR)/bootstrap ./cmd/sync

clean:
	rm -rf .aws-sam
