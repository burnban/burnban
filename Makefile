.PHONY: build test vet run clean

build:
	CGO_ENABLED=0 go build -o burnban .

test:
	go test ./...

vet:
	go vet ./...

run: build
	./burnban serve

clean:
	rm -f burnban
