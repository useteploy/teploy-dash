build:
	go build -o teploy-dash ./cmd/teploy-dash

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f teploy-dash
