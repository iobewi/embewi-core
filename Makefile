BINARY   = embewi-core
IMG      ?= embewi/core:latest

.PHONY: build tidy generate manifests docker-build

build:
	go build -o bin/$(BINARY) ./cmd/controller/

tidy:
	go mod tidy

# Génère le DeepCopy (nécessite controller-gen).
generate:
	controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

# Génère les CRDs YAML depuis les markers kubebuilder.
manifests:
	controller-gen crd paths="./..." output:crd:artifacts:config=config/crd/bases

docker-build:
	docker build -t $(IMG) .
