all: install

GOBIN ?= $(shell go env GOPATH)/bin

build:
	go build -o bin/kubectl-hns-list cmd/main.go

install: build
	cp bin/kubectl-hns-list $(GOBIN)/kubectl-hns-list
