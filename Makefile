.PHONY: all build test clean

BINARY_NAME=flar

all: build

build:
	go build -o $(BINARY_NAME) .

test:
	go test -v ./...

clean:
	rm -f $(BINARY_NAME)
