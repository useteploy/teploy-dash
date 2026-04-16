build:
	go build -o teploy-ui ./cmd/teploy-ui

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f teploy-ui
