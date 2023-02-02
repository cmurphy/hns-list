all: cli server

GOBIN ?= $(shell go env GOPATH)/bin

build-cli:
	cd cli; go build -o ../bin/kubectl-hns-list cmd/main.go

install-cli: build-cli
	cp bin/kubectl-hns-list $(GOBIN)/kubectl-hns-list

cli: install-cli

.PHONY: server
server:
	docker build . -t hns-list:latest
